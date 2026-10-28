package bootstrap

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"netshare/internal/control"
	"netshare/internal/enrollment"
)

func inviteCommand(args []string) error {
	flags := flag.NewFlagSet("netshare node invite", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "控制台配置")
	pairing := flags.String("pairing", "", "单节点四叶子安装包")
	dir := flags.String("dir", "", "密文交接目录；默认控制台状态目录下的 enrollments")
	ttl := flags.Duration("ttl", 15*time.Minute, "一次性接入码有效期，最长 1 小时")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *cfgPath == "" || *pairing == "" {
		return fmt.Errorf("用法：netshare node invite --config 配置 --pairing 节点安装包 [--dir 密文目录] [--ttl 15m]")
	}
	cfg, err := control.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *dir == "" {
		*dir = enrollment.Directory(cfg.StateFile)
	}
	receipt, err := enrollment.Create(cfg.ExternalURL, cfg.AccessPath, *pairing, *dir, *ttl)
	if err != nil {
		return err
	}
	// 仅 root 的父管理脚本捕获此 JSON；HTTP 服务端永远不保存密钥。
	return json.NewEncoder(os.Stdout).Encode(receipt)
}

func fetchCommand(args []string) error {
	flags := flag.NewFlagSet("netshare node fetch", flag.ContinueOnError)
	invitation := flags.String("invitation", "", "完整一次性接入码")
	stdin := flags.Bool("invitation-stdin", false, "从标准输入读取接入码，避免解密密钥出现在子进程参数中")
	endpoint := flags.String("url", "", "独立接入下载地址")
	key := flags.String("key", "", "独立解密密钥")
	dir := flags.String("dir", "", "尚不存在的私密输出目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *dir == "" {
		return fmt.Errorf("用法：netshare node fetch --invitation-stdin --dir 新目录")
	}
	if *stdin {
		if *invitation != "" || *endpoint != "" || *key != "" {
			return fmt.Errorf("--invitation-stdin 不能与其它接入参数同时使用")
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 8193))
		if err != nil || len(data) > 8192 {
			return fmt.Errorf("无法读取有效接入码")
		}
		*invitation = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	}
	if *invitation != "" {
		if *endpoint != "" || *key != "" {
			return fmt.Errorf("--invitation 不能与 --url/--key 同时使用")
		}
		var err error
		*endpoint, *key, err = enrollment.ParseInvitation(*invitation)
		if err != nil {
			return err
		}
	}
	if *endpoint == "" || *key == "" {
		return fmt.Errorf("请提供一次性接入码")
	}
	if err := enrollment.Fetch(context.Background(), *endpoint, *key, *dir); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "节点安装包已认证并保存。")
	return nil
}
