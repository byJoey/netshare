package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"netshare/internal/agent"
	"netshare/internal/bootstrap"
	"netshare/internal/control"
	"netshare/internal/model"
	"netshare/internal/webui"
)

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: netshare init|serve|agent|version")
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Println("NetShare " + model.Version)
		return nil
	case "init":
		return bootstrap.Run(args[1:])
	case "serve", "agent":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		path := flags.String("config", "", "配置文件路径")
		if e := flags.Parse(args[1:]); e != nil {
			return e
		}
		if *path == "" {
			return fmt.Errorf("必须通过 --config 指定配置文件")
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("存在未识别的参数")
		}
		if args[0] == "agent" {
			return agent.Run(ctx, *path)
		}
		assets, e := fs.Sub(webui.FS, "assets")
		if e != nil {
			return e
		}
		return control.Run(ctx, *path, assets)
	default:
		return fmt.Errorf("未知命令 %q；可用 init、serve、agent、version", args[0])
	}
}
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := run(ctx, os.Args[1:]); e != nil {
		slog.Error("服务退出", "error", e)
		os.Exit(1)
	}
}
