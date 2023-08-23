package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// RunCmdで使う構造体
type CmdArg struct {
	Cmd   []string
	Attr  syscall.ProcAttr
	SigCh chan os.Signal
}

func main() {
	loopCnt := 0
	for {
		var ca CmdArg

		// シグナル初期化
		ca.SigCh = make(chan os.Signal, 1)
		signal.Notify(ca.SigCh, syscall.SIGINT)

		// プロンプト表示
		fmt.Printf("./myshell[%d]> ", loopCnt)

		// 入力を3項間演算子でパース
		cmd, err := ParseInput()
		if err != io.EOF && err != nil {
			log.Print(err)
		}

		// 何も入力されなければcontinue
		if len(cmd) == 0 {
			loopCnt++
			continue
		}

		// シェル終了
		if err == io.EOF {
			break
		}
		if len(cmd) == 1 && cmd[0] == "bye" {
			break
		}

		// シェル実行
		ca.Shell(cmd)

		loopCnt++
	}
}

// cmd?yes:noを処理
// cmd ? b ? yb : nb : c ? yc : ncのようなネストされた3項間にも対応
func (ca *CmdArg) Shell(cmd []string) (*os.ProcessState, error) {
	// 入力を3項間演算子でparse
	cmd, yes, no := ParseTernaryOperator(cmd)

	// シェル実行
	status, err := ca.ShellMain(cmd)
	if err != nil {
		log.Print(err)
	}

	// SIGINT 割り込み
	go func() {
		select {
		case <-ca.SigCh:
			fmt.Println("(SIGINT caught!)")
			fmt.Printf("process %d exited with status(%d)\n", status.Pid(), status.ExitCode())
		}
	}()

	// 最初のコマンドの実行結果に応じて2番目3番目のコマンドを実行
	isTernOp := bool(yes != nil && no != nil)
	if isTernOp {
		if status.Success() {
			yca := CmdArg{}
			_, err := yca.Shell(yes)
			if err != nil {
				log.Print(err)
			}
		} else {
			nca := CmdArg{}
			_, err := nca.Shell(no)
			if err != nil {
				log.Print(err)
			}
		}
	}

	return nil, nil
}

// 3項間で分けられたコマンド、パイプ、リダイレクトの処理
func (ca *CmdArg) ShellMain(args []string) (*os.ProcessState, error) {
	// A|B|C|DをA|B|CとDに分ける
	args1, args2 := ParsePipe(args)

	// redirectをパース
	err := ca.ParseRedirect(args2)
	if err != nil {
		return nil, err
	}

	// パイプがある場合の処理
	if len(args1) > 0 {
		// A|B|Cの処理結果を返す
		in, err := ca.ProcessPipe(args1)
		defer in.Close()
		if err != nil {
			return nil, err
		}
		ca.Attr.Files[0] = in.Fd()
	}

	return RunCmd(*ca)
}

// パイプを再帰的に処理する
func (ca *CmdArg) ProcessPipe(args []string) (*os.File, error) {
	// A|B|CをA|BとCに分ける
	args1, args2 := ParsePipe(args)

	// parse redirect
	err := ca.ParseRedirect(args2)
	if err != nil {
		return nil, err
	}

	// make a pipe
	pin, pout, err := os.Pipe()
	defer pout.Close()
	ca.Attr.Files[1] = pout.Fd()

	// まだパイプが残ってるとき
	if len(args1) > 0 {
		// 再帰的にパイプを処理
		in, err := ca.ProcessPipe(args1)
		defer in.Close()
		if err != nil {
			return nil, err
		}
		ca.Attr.Files[0] = in.Fd()
	}

	// run command
	_, err = RunCmd(*ca)
	if err != nil {
		return nil, err
	}

	// 出力先を返す
	return pin, nil
}

// 引数のコマンドを実行
func RunCmd(ca CmdArg) (*os.ProcessState, error) {
	// 入力したコマンドが存在するか確認
	cpath, err := exec.LookPath(ca.Cmd[0])
	if err != nil {
		return nil, err
	}

	// コマンド実行
	pid, err := syscall.ForkExec(cpath, ca.Cmd, &ca.Attr)
	if err != nil {
		return nil, err
	}

	// 実行したプロセスの状態を取得
	proc, _ := os.FindProcess(pid)

	go func() {
		select {
		case s := <-ca.SigCh:
			proc.Signal(s)
			ca.SigCh <- s
		}
	}()

	// 実行が終わるまで待つ
	status, err := proc.Wait()
	if err != nil {
		return nil, err
	}

	// 成功しなければメッセージを出力
	if !status.Success() {
		fmt.Println(status.String())
	}

	return status, nil
}

/*
	入力等のパース処理
*/
// プロンプトに入力された文字列をパース
func ParseInput() ([]string, error) {
	// 標準入力
	scanner := bufio.NewScanner(os.Stdin)

	// EOFチェック
	if !scanner.Scan() {
		return nil, io.EOF
	}
	line := scanner.Text()

	// 入力を分離記号で分離
	sep := []string{" ", "?", ":", "<", ">", "2>", "|"}
	args := SplitMultiSep(line, sep)
	args = SkipWhiteSpace(args)

	return args, nil
}

// argsを?と:で分ける
// (A ? (B ? y : n) : (C ? y : n))にも対応したい
func ParseTernaryOperator(args []string) ([]string, []string, []string) {
	// yesとnoの開始位置
	n := len(args)
	yi := n
	ni := n

	cnt := 0
	// yiとniを決定
	for i, a := range args {
		if a == "?" {
			cnt += 1
		}
		if a == ":" {
			cnt -= 1
		}
		if yi == n && cnt == 1 {
			yi = i
		}
		if yi != n && ni == n && cnt == 0 {
			ni = i
			break
		}
	}

	var cmd, yes, no []string
	// cmd
	cmd = make([]string, yi)
	copy(cmd, args[:yi])

	if yi != n && ni != n {
		// yes
		yes = make([]string, ni-yi-1)
		copy(yes, args[yi+1:ni])

		// no
		no = args[ni+1:]
	}

	return cmd, yes, no
}

// リダイレクトをパース
func (ca *CmdArg) ParseRedirect(cmd []string) error {
	// 変数初期化
	in := os.Stdin
	out := os.Stdout
	err := os.Stderr
	var newCmd []string
	var perr error

	i := 0
	// commandを取得
	for i = 0; i < len(cmd); i++ {
		// リダイレクト記号が来たらbreak
		if cmd[i] == "<" || cmd[i] == ">" || cmd[i] == "2>" {
			break
		}
		// white-space以外ならnewCmdに追加
		if cmd[i] != "" && cmd[i] != " " && cmd[i] != "\t" && cmd[i] != "\n" {
			newCmd = append(newCmd, cmd[i])
		}
	}

	// リダイレクト先を取得
	for ; i < len(cmd); i++ {
		if cmd[i] == "<" {
			in, perr = os.OpenFile(cmd[i+1], os.O_RDONLY, 0666)
			if perr != nil {
				return perr
			}
		}
		if cmd[i] == ">" {
			out, perr = os.OpenFile(cmd[i+1], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
			if perr != nil {
				return perr
			}
		}
		if cmd[i] == "2>" {
			err, perr = os.OpenFile(cmd[i+1], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
			if perr != nil {
				return perr
			}
		}
	}

	// リダイレクト先をattrに設定
	// デフォルト値はstdin, stdout, stderr
	ca.Cmd = newCmd
	ca.Attr = syscall.ProcAttr{
		Files: []uintptr{in.Fd(), out.Fd(), err.Fd()},
	}
	return nil
}

// A|B|C|DをA|B|CとDに分ける
func ParsePipe(args []string) ([]string, []string) {
	var args1, args2 []string

	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == "|" {
			args1 = make([]string, i)
			copy(args1, args[:i])
			args2 = args[i+1:]
			break
		}
		if i == 0 {
			args2 = args
		}
	}

	return args1, args2
}

// 入力を分離記号で分割する(ref: https://qiita.com/yoya/items/23ac2c490625c5d47ad9)
func SplitMultiSep(s string, sep []string) []string {
	var ret []string
	ret = Split(s, sep[0])
	if len(sep) > 1 {
		ret2 := []string{}
		for _, r := range ret {
			ret2 = append(ret2, SplitMultiSep(r, sep[1:])...)
		}
		ret = ret2
	}
	return ret
}

// sepを残したstrings.Split
// ref: https://teratail.com/questions/345393
func Split(s, sep string) (out []string) {

	for len(s) > 0 {
		i := strings.Index(s, sep)
		if i == -1 {
			out = append(out, s)
			break
		}

		out = append(out, s[:i])
		out = append(out, sep)
		s = s[i+1:]
	}
	return out
}

// whitespaceはskip
func SkipWhiteSpace(s []string) []string {
	var out []string
	for _, si := range s {
		if si == " " {
			continue
		}
		out = append(out, si)
	}
	return out
}
