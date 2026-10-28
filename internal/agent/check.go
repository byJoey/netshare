package agent

import (
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// CheckCommand 复用节点启动校验，验证配对资料与本机地址，不应用 nft 规则。
func CheckCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("netshare check-agent", flag.ContinueOnError)
	path := flags.String("config", "", "待安装的节点配置")
	dir := flags.String("credential-dir", "", "配对包中的证书目录")
	state := flags.String("state-file", "", "可选：只读检查原节点状态是否与新配置兼容")
	skip := flags.Bool("skip-address-check", false, "仅在隔离测试中跳过本机 IP/网卡核对")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" {
		return fmt.Errorf("用法：netshare check-agent --config 配置 [--credential-dir 叶子目录] [--state-file 原状态]")
	}
	data, err := os.ReadFile(*path)
	if err != nil || len(data) > maxBodyBytes*2 {
		return fmt.Errorf("读取节点配置失败或文件过大")
	}
	var cfg Config
	if err = decodeStrict(data, &cfg); err != nil {
		return fmt.Errorf("节点配置格式错误：%w", err)
	}
	if *dir != "" {
		cfg.CAFile = filepath.Join(*dir, "ca.pem")
		cfg.CertFile = filepath.Join(*dir, "server.pem")
		cfg.KeyFile = filepath.Join(*dir, "server-key.pem")
	}
	if *state != "" {
		info, err := os.Lstat(*state)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("现有节点状态必须是可读取的普通文件")
		}
		cfg.StateFile = *state
	} else {
		// 不读取发行包配置里的真实系统状态，也不写任何节点状态。
		tmp, err := os.MkdirTemp("", "netshare-agent-check-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		cfg.StateFile = filepath.Join(tmp, "absent-state.json")
	}
	s, err := NewServer(cfg, nil)
	if err != nil {
		return err
	}
	for _, path := range []string{cfg.CAFile, cfg.CertFile, cfg.KeyFile} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("节点证书和私钥必须是普通文件")
		}
	}
	tlsConfig, err := loadTLS(cfg)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	if err != nil || leaf.Subject.CommonName != cfg.Node.ID {
		return fmt.Errorf("节点证书身份与节点 ID 不符")
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: tlsConfig.ClientCAs, DNSName: cfg.Node.PrivateIP, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("节点证书与 CA、私网 IP 不匹配或已过期：%w", err)
	}
	if !*skip {
		if err = s.checkAddresses(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stdout, "节点配对资料检查通过。")
	return nil
}
