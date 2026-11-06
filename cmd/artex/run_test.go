package main

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// withRestartSignal 把进程级重启信号换成一个测试能控制的 channel。
// run() 里那条路径等价于"升级/回滚已完成，请退出让我重启"，退出码是
// 75（selfupdate.ExitRestart），守护脚本据此立刻重新拉起。
// chdirTemp 把工作目录挪到临时目录。server 会按相对路径落地运行时文件
// （如 jwt.key），不隔离就会往仓库里写垃圾。
func chdirTemp(t *testing.T) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取工作目录: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("切换工作目录: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func withRestartSignal(t *testing.T) chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	old := restartRequested
	restartRequested = func() <-chan struct{} { return ch }
	t.Cleanup(func() { restartRequested = old })
	return ch
}

// TestRunBootsAndExitsWithRestartCode 是 main 的端到端冒烟：真实建库、真实监听、
// 真实优雅关闭，只是在"该重启了"那一刻由测试触发。
//
// 需要 PG（ARTEX_PG_DSN）；未配置时跳过，与其它依赖数据库的包保持一致。
func TestRunBootsAndExitsWithRestartCode(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("ARTEX_PG_DSN 未配置，跳过需要数据库的启动用例")
	}

	chdirTemp(t)
	ch := withRestartSignal(t)
	oldArgs := os.Args
	// run() 用全局 flag 集合解析参数，测试里必须换成自己的，否则 -test.* 会被
	// 当成未知参数直接退出进程。端口给 0 由内核分配，避免和本机实例撞车。
	os.Args = []string{"artex", "-addr", "127.0.0.1:0", "-data", t.TempDir(), "-proxy", ""}
	t.Cleanup(func() { os.Args = oldArgs })

	go func() {
		time.Sleep(300 * time.Millisecond)
		close(ch)
	}()

	code := run()
	if code != selfupdate.ExitRestart {
		t.Fatalf("升级后重启的退出码应为 %d，得到 %d", selfupdate.ExitRestart, code)
	}
}

// TestRunReturnsZeroOnSignalShutdown 覆盖"用户正常停止"：退出码 0 让守护脚本
// 结束循环，而不是把它当成崩溃再拉起来。
func TestRunReturnsZeroOnSignalShutdown(t *testing.T) {
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("ARTEX_PG_DSN 未配置，跳过需要数据库的启动用例")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持向自身投递 os.Interrupt")
	}

	// 不关重启通道，改用 os.Interrupt：run() 的信号上下文走 ctx.Done() 分支。
	chdirTemp(t)
	_ = withRestartSignal(t)
	oldArgs := os.Args
	os.Args = []string{"artex", "-addr", "127.0.0.1:0", "-data", t.TempDir(), "-proxy", ""}
	t.Cleanup(func() { os.Args = oldArgs })

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Skipf("无法向自身发送信号: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = proc.Signal(os.Interrupt)
	}()

	done := make(chan int, 1)
	go func() { done <- run() }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("正常停止应返回 0，得到 %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("收到中断后 30s 仍未退出，优雅关闭可能卡住")
	}
}
