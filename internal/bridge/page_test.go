package bridge

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderUsesNonceAndConfiguredBatch(t *testing.T) {
	page, err := Render("proxy.example.com", "bootstrap-token", 2*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	body := string(page.Body)
	if page.Nonce == "" || !strings.Contains(body, `script nonce="`+page.Nonce+`"`) || !strings.Contains(page.CSP, `script-src 'nonce-`+page.Nonce+`'`) {
		t.Fatal("rendered bridge does not bind its script to the response nonce")
	}
	if !strings.Contains(body, "batchLimit=2097152") {
		t.Fatal("rendered bridge omitted the configured carrier batch")
	}
	if !strings.Contains(body, "queueItemLimit=16384") || !strings.Contains(body, "setTimeout(abort,90000)") {
		t.Fatal("rendered bridge omitted its item or request bound")
	}
	for _, unwanted := range [][]byte{[]byte("pause(0)"), []byte(".slice(0)"), []byte("__BATCH_LIMIT__")} {
		if bytes.Contains(page.Body, unwanted) {
			t.Fatalf("rendered bridge retained %q", unwanted)
		}
	}
}

func TestRenderRejectsInvalidBatch(t *testing.T) {
	if _, err := Render("proxy.example.com", "bootstrap-token", 0); err == nil {
		t.Fatal("accepted a nonpositive carrier batch")
	}
}
