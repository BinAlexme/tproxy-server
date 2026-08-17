package bridge

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderUsesNonceAndConfiguredBatch(t *testing.T) {
	page, err := Render("proxy.example.com", "bootstrap-token", "https", 2*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	body := string(page.Body)
	if page.Nonce == "" || !strings.Contains(body, `script nonce="`+page.Nonce+`"`) || !strings.Contains(page.CSP, `script-src 'nonce-`+page.Nonce+`'`) {
		t.Fatal("rendered bridge does not bind its script to the response nonce")
	}
	if !strings.Contains(body, "carrierMode=\"https\"") || !strings.Contains(body, "batchLimit=2097152") {
		t.Fatal("rendered bridge omitted the configured carrier batch")
	}
	if !strings.Contains(body, "queueItemLimit=16384") || !strings.Contains(body, "setTimeout(abort,90000)") {
		t.Fatal("rendered bridge omitted its item or request bound")
	}
	if !strings.Contains(body, "fetch(relayOrigin+path,requestOptions)") ||
		!strings.Contains(body, "mode:'same-origin',credentials:'omit'") ||
		!strings.Contains(body, "cache:'no-store',redirect:'error',referrerPolicy:'no-referrer'") {
		t.Fatal("rendered bridge omitted its same-origin request restrictions")
	}
	if !strings.Contains(body, "t:'traffic',up:batch.total,down:0") || !strings.Contains(body, "t:'traffic',up:0,down:data.byteLength") {
		t.Fatal("rendered bridge omitted acknowledged traffic counters")
	}
	if !strings.Contains(body, "t:'tproxy-android-init',v:1,nonce:androidNonce") ||
		!strings.Contains(body, "globalThis.TelegramWebProxy") ||
		!strings.Contains(body, "androidBridge.postMessage(frame.data)") {
		t.Fatal("rendered bridge omitted the origin-scoped Android transport")
	}
	for _, unwanted := range [][]byte{
		[]byte("pause(0)"),
		[]byte(".slice(0)"),
		[]byte("__BATCH_LIMIT__"),
		[]byte("localStorage"),
		[]byte("sessionStorage"),
		[]byte("indexedDB"),
		[]byte("serviceWorker"),
		[]byte("new Worker"),
		[]byte("document.cookie"),
		[]byte("<iframe"),
		[]byte("<img"),
		[]byte("<link"),
		[]byte("<style"),
		[]byte("<audio"),
		[]byte("<video"),
		[]byte("<object"),
	} {
		if bytes.Contains(page.Body, unwanted) {
			t.Fatalf("rendered bridge retained %q", unwanted)
		}
	}
}

func TestRenderUsesHardenedExecutionPolicy(t *testing.T) {
	page, err := Render("proxy.example.com", "bootstrap-token", "https", 2*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"default-src":     "'none'",
		"base-uri":        "'none'",
		"child-src":       "'none'",
		"connect-src":     "'self' wss://proxy.example.com",
		"font-src":        "'none'",
		"form-action":     "'none'",
		"frame-ancestors": "http://127.0.0.1:*",
		"frame-src":       "'none'",
		"img-src":         "'none'",
		"manifest-src":    "'none'",
		"media-src":       "'none'",
		"object-src":      "'none'",
		"script-src":      "'nonce-" + page.Nonce + "'",
		"style-src":       "'none'",
		"worker-src":      "'none'",
		"sandbox":         "allow-same-origin allow-scripts",
	}
	directives := make(map[string]string)
	for _, directive := range strings.Split(page.CSP, "; ") {
		name, value, ok := strings.Cut(directive, " ")
		if !ok {
			t.Fatalf("invalid CSP directive %q", directive)
		}
		directives[name] = value
	}
	if len(directives) != len(expected) {
		t.Fatalf("CSP has %d directives, want %d: %q", len(directives), len(expected), page.CSP)
	}
	for name, value := range expected {
		if directives[name] != value {
			t.Fatalf("CSP directive %q is %q, want %q", name, directives[name], value)
		}
	}

	for _, feature := range []string{
		"autoplay=()",
		"camera=()",
		"clipboard-read=()",
		"clipboard-write=()",
		"display-capture=()",
		"geolocation=()",
		"microphone=()",
		"payment=()",
		"screen-wake-lock=()",
		"usb=()",
	} {
		if !strings.Contains(PermissionsPolicy, feature) {
			t.Fatalf("permissions policy does not deny %q", feature)
		}
	}
}

func TestRenderRejectsInvalidBatch(t *testing.T) {
	if _, err := Render("proxy.example.com", "bootstrap-token", "https", 0); err == nil {
		t.Fatal("accepted a nonpositive carrier batch")
	}
	if _, err := Render("proxy.example.com", "bootstrap-token", "invalid", 2*1024*1024); err == nil {
		t.Fatal("accepted an invalid carrier mode")
	}
}

func TestRenderIncludesSelectableCarrierImplementations(t *testing.T) {
	for _, mode := range []string{"https", "https-lanes", "websocket"} {
		page, err := Render("proxy.example.com", "bootstrap-token", mode, 2*1024*1024)
		if err != nil {
			t.Fatal(err)
		}
		body := string(page.Body)
		if !strings.Contains(body, `carrierMode="`+mode+`"`) {
			t.Fatalf("rendered bridge omitted carrier mode %q", mode)
		}
		for _, implementation := range []string{
			"async function runLaneUp(lane)",
			"async function pollLane(lane)",
			"function openWebSocket()",
			"function runWebSocketUp()",
		} {
			if !strings.Contains(body, implementation) {
				t.Fatalf("rendered %s bridge omitted %q", mode, implementation)
			}
		}
	}
}
