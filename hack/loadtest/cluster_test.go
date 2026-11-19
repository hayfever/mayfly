package main

import (
	"reflect"
	"testing"
)

func TestForwardPort(t *testing.T) {
	cases := []struct {
		line   string
		port   string
		wanted bool
	}{
		{"Forwarding from 127.0.0.1:51234 -> 8080", "51234", true},
		{"Forwarding from [::1]:51235 -> 8080", "51235", true},
		{"Handling connection for 8080", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		port, ok := forwardPort(tc.line)
		if ok != tc.wanted || port != tc.port {
			t.Errorf("forwardPort(%q) = %q/%v, want %q/%v", tc.line, port, ok, tc.port, tc.wanted)
		}
	}
}

func TestKubectlArgs(t *testing.T) {
	opts := options{
		kubeconfig:  "/tmp/kubeconfig",
		kubeContext: "k3d-mayfly",
		namespace:   "mayfly",
		service:     "mayfly",
	}
	want := []string{
		"--kubeconfig", "/tmp/kubeconfig",
		"--context", "k3d-mayfly",
		"--namespace", "mayfly",
		"port-forward", "svc/mayfly", ":8080",
	}
	if got := kubectlArgs(opts); !reflect.DeepEqual(got, want) {
		t.Errorf("kubectlArgs = %v, want %v", got, want)
	}
}
