package model

import "testing"

func TestPortRangeValidation(t *testing.T) {
	for _, s := range []string{"0", "65536", "9-8", "1-1001", "1-2-3", "22;flush ruleset", "-1", "80\n90"} {
		if _, e := ParsePorts(s); e == nil {
			t.Fatalf("不应接受 %q", s)
		}
	}
	for _, s := range []string{"80", "1-1000", "65535"} {
		if _, e := ParsePorts(s); e != nil {
			t.Fatal(s, e)
		}
	}
	if e := ValidatePorts("both", "100-102", "200-202"); e != nil {
		t.Fatal(e)
	}
	if e := ValidatePorts("icmp", "80", "80"); e == nil {
		t.Fatal("不应接受不支持的协议")
	}
}
