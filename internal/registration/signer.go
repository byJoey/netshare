package registration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"netshare/internal/model"
)

type SignRequest struct {
	Node model.Node `json:"node"`
	CSR  []byte     `json:"csr"`
}

type SignReply struct {
	Certificate []byte `json:"certificate"`
}

func SafePath(value string) error {
	if !filepath.IsAbs(value) {
		return fmt.Errorf("受管路径必须是绝对路径")
	}
	for p := filepath.Clean(value); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("受管路径不能包含符号链接")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

func WritePrivate(file string, data []byte) error {
	if err := SafePath(file); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".join-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), file)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func privateRead(file string) ([]byte, error) {
	if err := SafePath(file); err != nil {
		return nil, err
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("签发资料必须是仅属主可读的普通文件")
	}
	return os.ReadFile(file)
}

func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err == nil && n.Sign() == 0 {
		n.SetInt64(1)
	}
	return n, err
}

// InitSigner 生成独立的节点服务端CA，不读取或迁移原控制器CA主私钥。
func InitSigner(dir string) error {
	if err := SafePath(dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "ca-key.pem")); err == nil {
		_, _, err = loadIssuer(dir)
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err == nil {
		return fmt.Errorf("接入签发资料不完整，请恢复备份，不能重新生成")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("接入签发目录必须仅root可访问")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	number, err := serial()
	if err != nil {
		return err
	}
	var ranges []*net.IPNet
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, block, _ := net.ParseCIDR(cidr)
		ranges = append(ranges, block)
	}
	now := time.Now().UTC()
	cert := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: "NetShare Node Enrollment CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, MaxPathLen: 0, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, PermittedIPRanges: ranges, PermittedDNSDomainsCritical: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err = WritePrivate(filepath.Join(dir, "ca-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	return WritePrivate(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func loadIssuer(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := privateRead(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := privateRead(filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		return nil, nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return nil, nil, fmt.Errorf("接入签发证书与私钥不匹配")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || !cert.IsCA || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || len(cert.PermittedIPRanges) != 3 || time.Now().After(cert.NotAfter) {
		return nil, nil, fmt.Errorf("接入签发CA必须仅用于私网节点服务端")
	}
	if err = cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf("接入签发CA无效")
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("接入签发私钥类型无效")
	}
	return cert, key, nil
}

func ParseCSR(node model.Node, data []byte) (*x509.CertificateRequest, error) {
	if err := model.ValidateNode(node); err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 || len(data) > 16<<10 {
		return nil, fmt.Errorf("节点身份证明无效")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil || csr.Subject.CommonName != node.ID || len(csr.IPAddresses) != 1 || !csr.IPAddresses[0].Equal(net.ParseIP(node.PrivateIP)) || len(csr.DNSNames) != 0 || len(csr.URIs) != 0 || len(csr.EmailAddresses) != 0 {
		return nil, fmt.Errorf("节点身份证明与上报地址不匹配")
	}
	key, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("节点密钥必须为P-256")
	}
	return csr, nil
}

func KeyID(public any) (string, error) {
	data, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func SignCertificate(dir string, request SignRequest) (SignReply, error) {
	csr, err := ParseCSR(request.Node, request.CSR)
	if err != nil {
		return SignReply{}, err
	}
	issuer, key, err := loadIssuer(dir)
	if err != nil {
		return SignReply{}, err
	}
	number, err := serial()
	if err != nil {
		return SignReply{}, err
	}
	now := time.Now().UTC()
	expires := now.AddDate(2, 0, 0)
	if expires.After(issuer.NotAfter) {
		expires = issuer.NotAfter
	}
	cert := &x509.Certificate{SerialNumber: number, Subject: pkix.Name{CommonName: request.Node.ID}, NotBefore: now.Add(-5 * time.Minute), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: csr.IPAddresses, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, issuer, csr.PublicKey, key)
	if err != nil {
		return SignReply{}, err
	}
	return SignReply{Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

func Sign(ctx context.Context, socket string, request SignRequest) (SignReply, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return SignReply{}, err
	}
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://signer/sign", bytes.NewReader(data))
	if err != nil {
		return SignReply{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return SignReply{}, fmt.Errorf("节点接入服务暂不可用，请稍后重试")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return SignReply{}, fmt.Errorf("节点身份证明未通过检查")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<10))
	if err != nil {
		return SignReply{}, err
	}
	var reply SignReply
	err = Decode(body, &reply)
	return reply, err
}

func SignerCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "init" && args[0] != "serve") {
		return fmt.Errorf("用法：netshare signer init|serve --dir 签发目录 [--socket 本机套接字]")
	}
	flags := flag.NewFlagSet("signer", flag.ContinueOnError)
	dir := flags.String("dir", "", "root专用接入签发目录")
	socket := flags.String("socket", "/run/netshare-sign/registration.sock", "仅本机Web用户可访问的套接字")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *dir == "" {
		return fmt.Errorf("缺少签发目录")
	}
	if args[0] == "init" {
		return InitSigner(*dir)
	}
	if _, _, err := loadIssuer(*dir); err != nil {
		return err
	}
	if err := SafePath(*socket); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*socket), 0750); err != nil {
		return err
	}
	if info, err := os.Lstat(*socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("签发套接字路径已被其他文件占用")
		}
		if err = os.Remove(*socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(*socket, 0660); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sign", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
		var in SignRequest
		if err != nil || Decode(data, &in) != nil {
			http.Error(w, "请求无效", 400)
			return
		}
		out, err := SignCertificate(*dir, in)
		if err != nil {
			http.Error(w, "身份证明无效", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 4096}
	go func() {
		<-ctx.Done()
		child, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(child)
	}()
	if err = srv.Serve(listener); errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
