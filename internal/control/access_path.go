package control

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"netshare/internal/model"
)

// AccessPathCommand 仅在管理员显式调用时显示或更换入口，不启动服务。
func AccessPathCommand(args []string, output io.Writer) error {
	if len(args) == 0 || args[0] != "show" && args[0] != "rotate" {
		return fmt.Errorf("用法：netshare access-path show|rotate --config 配置文件")
	}
	flags := flag.NewFlagSet("access-path", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "控制器配置文件")
	if err := flags.Parse(args[1:]); err != nil {
		return fmt.Errorf("管理入口参数无效，请使用 --config 指定配置文件")
	}
	if *configPath == "" || flags.NArg() != 0 {
		return fmt.Errorf("必须通过 --config 指定控制器配置文件，不接受其他位置参数")
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if args[0] == "rotate" {
		cfg.AccessPath, err = model.NewAccessPath()
		if err != nil {
			return err
		}
		if err = saveConfigPreservingPermissions(*configPath, cfg); err != nil {
			return fmt.Errorf("保存管理入口失败：%w", err)
		}
	}
	_, err = fmt.Fprintln(output, strings.TrimRight(cfg.ExternalURL, "/")+cfg.AccessPath+"/")
	return err
}

func saveConfigPreservingPermissions(path string, cfg Config) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("控制器配置必须是普通文件，不能使用符号链接")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法读取原配置的所有者，未修改文件")
	}
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".netshare-access-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	// root 执行维护命令后仍需让原服务用户读取配置。
	if err = file.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
		return fmt.Errorf("保留配置所有者失败：%w", err)
	}
	if err = file.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(cfg); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
