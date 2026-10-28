package control

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"netshare/internal/enrollment"
)

func TestEnrollmentRequiresSecretPrefixButKeepsOtherAPIAuthentication(t *testing.T) {
	c, _ := testController(t)
	actual, err := filepath.EvalSymlinks(filepath.Dir(c.cfg.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.StateFile = filepath.Join(actual, "state.json")
	c.cfg.AccessPath = "/ns-test0123456789012345"
	server := httptest.NewServer(c.Handler(nil))
	defer server.Close()
	c.cfg.ExternalURL = server.URL
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"agent.json", "ca.pem", "server.pem", "server-key.pem"} {
		body := []byte("测试节点叶子")
		tw.WriteHeader(&tar.Header{Name: "edge-a/" + name, Mode: 0600, Size: int64(len(body))})
		tw.Write(body)
	}
	tw.Close()
	gz.Close()
	pairing := filepath.Join(actual, "pairing.tar.gz")
	if err = os.WriteFile(pairing, compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	receipt, err := enrollment.Create(server.URL, c.cfg.AccessPath, pairing, enrollment.Directory(c.cfg.StateFile), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	check := func(method, url string, want int) {
		t.Helper()
		req, _ := http.NewRequest(method, url, nil)
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s: got %d want %d", method, res.StatusCode, want)
		}
	}
	check(http.MethodGet, server.URL+c.cfg.AccessPath+"/api/overview", http.StatusUnauthorized)
	check(http.MethodPost, server.URL+"/api/enroll/"+filepath.Base(receipt.URL), http.StatusNotFound)
	check(http.MethodGet, receipt.URL, http.StatusNotFound)
	check(http.MethodPost, receipt.URL, http.StatusOK)
	check(http.MethodPost, receipt.URL, http.StatusNotFound)
	check(http.MethodGet, server.URL+c.cfg.AccessPath+"/api/overview", http.StatusUnauthorized)
}
