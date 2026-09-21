package service

import "testing"

func TestParseLsofCWDFiltersMountDescendants(t *testing.T) {
	out := []byte("p12\ncbash\nn/work/cloud\np13\nccodex\nn/work/cloud/project\np14\ncother\nn/work/cloud-old\n")
	holders := parseLsofCWD(out, "/work/cloud")
	if len(holders) != 2 {
		t.Fatalf("holders = %+v", holders)
	}
	if holders[0].PID != 12 || holders[0].Command != "bash" || holders[0].Path != "/work/cloud" {
		t.Fatalf("root holder = %+v", holders[0])
	}
	if holders[1].PID != 13 || holders[1].Command != "codex" || holders[1].Path != "/work/cloud/project" {
		t.Fatalf("descendant holder = %+v", holders[1])
	}
}
