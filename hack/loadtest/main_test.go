package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseOptionsDefaults(t *testing.T) {
	opts, err := parseOptions(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.mode != "sessions" || opts.concurrency != 5 || opts.duration != 2*time.Minute {
		t.Errorf("defaults = %s/%d/%s, want sessions/5/2m", opts.mode, opts.concurrency, opts.duration)
	}
	if opts.namespace != "mayfly" || opts.service != "mayfly" {
		t.Errorf("target defaults = %s/%s, want mayfly/mayfly", opts.namespace, opts.service)
	}
	if opts.tokenEnv != "MAYFLY_API_TOKEN" || opts.webhookTrigger != "generic" {
		t.Errorf("env defaults = %s/%s", opts.tokenEnv, opts.webhookTrigger)
	}
}

func TestParseOptionsRejects(t *testing.T) {
	cases := [][]string{
		{"-mode", "flood"},
		{"-n", "0"},
		{"-duration", "0s"},
		{"-iterations", "-1"},
		{"-url", "ftp://example.test"},
		{"-url", "not-a-url"},
		{"-mode", "webhooks", "-webhook-trigger", ""},
		{"-ttl", "banana"},
		{"-session-timeout", "1ms", "-poll-interval", "1s"},
		{"extra"},
	}
	for _, args := range cases {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Errorf("parseOptions(%v) accepted an invalid configuration", args)
		}
	}
}

func TestDryRunPrintsPlanWithoutContactingTarget(t *testing.T) {
	t.Setenv("MAYFLY_API_TOKEN", "")
	t.Setenv("MAYFLY_WEBHOOK_SECRET", "")
	var stdout, stderr bytes.Buffer
	code := realMain([]string{
		"-dry-run", "-mode", "both", "-context", "k3d-mayfly", "-n", "7", "-duration", "90s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr.String())
	}
	for _, want := range []string{"workers:     7", "window:      1m30s", "k3d-mayfly", "generic", "not set"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("plan does not contain %q:\n%s", want, stdout.String())
		}
	}
}

func TestPlanNeverPrintsTheToken(t *testing.T) {
	t.Setenv("MAYFLY_API_TOKEN", "super-secret-token")
	t.Setenv("MAYFLY_WEBHOOK_SECRET", "super-secret-hook")
	opts, err := parseOptions([]string{"-mode", "both"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printPlan(&out, opts, func(key string) string {
		switch key {
		case "MAYFLY_API_TOKEN", "MAYFLY_WEBHOOK_SECRET":
			return "super-secret"
		}
		return ""
	})
	if strings.Contains(out.String(), "super-secret") {
		t.Errorf("plan leaked a secret value:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "set") {
		t.Errorf("plan does not report the configured secrets:\n%s", out.String())
	}
}

func TestMissingTokenFailsBeforeConnecting(t *testing.T) {
	t.Setenv("MAYFLY_API_TOKEN", "")
	var stdout, stderr bytes.Buffer
	code := realMain([]string{"-url", "http://127.0.0.1:1"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "MAYFLY_API_TOKEN") {
		t.Errorf("stderr does not name the token variable:\n%s", stderr.String())
	}
}
