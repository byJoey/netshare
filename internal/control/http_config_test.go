package control

import (
	"testing"
)

func TestLoadConfigDirectHTTPAndHTTPSCompatibility(t *testing.T) {
	original, path := testAccessConfig(t)
	for _, tc := range []struct {
		name   string
		listen string
		origin string
		secure bool
		valid  bool
	}{
		{"旧HTTPS反代", "127.0.0.1:18440", "https://netshare.example.test", true, true},
		{"IP直连HTTP", "0.0.0.0:18440", "http://192.0.2.10:18440", false, true},
		{"明确IPv4直连", "192.0.2.10:18440", "http://192.0.2.10:18440", false, true},
		{"HTTP回环", "127.0.0.1:18440", "http://127.0.0.1:18440", false, true},
		{"拒绝HTTP安全Cookie", "0.0.0.0:18440", "http://192.0.2.10:18440", true, false},
		{"拒绝HTTPS公网明文监听", "0.0.0.0:18440", "https://netshare.example.test", true, false},
		{"拒绝HTTPS不安全Cookie公网", "0.0.0.0:18440", "https://netshare.example.test", false, false},
		{"拒绝隐式所有IP", ":18440", "http://192.0.2.10:18440", false, false},
		{"拒绝IPv6公网", "[::]:18440", "http://192.0.2.10:18440", false, false},
		{"拒绝无效端口", "0.0.0.0:70000", "http://192.0.2.10:18440", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := original
			cfg.Listen, cfg.ExternalURL, cfg.CookieSecure = tc.listen, tc.origin, tc.secure
			if err := atomicJSON(path, cfg); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
