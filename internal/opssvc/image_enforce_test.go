package opssvc

import (
	"testing"

	"connectrpc.com/connect/v2"
)

// TestEnforceSandboxImage covers the accept/reject/rewrite matrix for the
// sandbox image guard (registry host = artifact, org = sandbox).
func TestEnforceSandboxImage(t *testing.T) {
	s := &Service{imageRegistryHost: "git.agent.svc.cluster.local", sandboxOrg: "sandbox"}

	cases := []struct {
		name    string
		in      string
		want    string // expected image after enforcement (when ok)
		wantErr bool
	}{
		{
			name: "artifact + sandbox org accepted as-is",
			in:   "git.agent.svc.cluster.local/sandbox/sandbox-base:debian-trixie",
			want: "git.agent.svc.cluster.local/sandbox/sandbox-base:debian-trixie",
		},
		{
			name: "bare org/name is rewritten to the configured registry",
			in:   "sandbox/sandbox-base:debian-trixie",
			want: "git.agent.svc.cluster.local/sandbox/sandbox-base:debian-trixie",
		},
		{
			name:    "wrong registry host rejected",
			in:      "docker.io/sandbox/sandbox-base:1",
			wantErr: true,
		},
		{
			name:    "wrong org rejected",
			in:      "git.agent.svc.cluster.local/other/sandbox-base:1",
			wantErr: true,
		},
		{
			name:    "bare single name has no org -> rejected",
			in:      "sandbox-base:1",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.enforceSandboxImage(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected rejection for %q, got %q", c.in, got)
				}
				if connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("rejection code = %v, want InvalidArgument", connect.CodeOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("enforceSandboxImage(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestEnforceServiceImage: a service image must come from artifact, but any org
// is allowed.
func TestEnforceServiceImage(t *testing.T) {
	s := &Service{imageRegistryHost: "git.agent.svc.cluster.local"}

	if got, err := s.enforceServiceImage("git.agent.svc.cluster.local/myorg/app:1"); err != nil || got != "git.agent.svc.cluster.local/myorg/app:1" {
		t.Fatalf("artifact image with any org must be accepted, got %q err=%v", got, err)
	}
	if got, err := s.enforceServiceImage("myorg/app:1"); err != nil || got != "git.agent.svc.cluster.local/myorg/app:1" {
		t.Fatalf("bare org/name must be rewritten, got %q err=%v", got, err)
	}
	if _, err := s.enforceServiceImage("docker.io/library/redis:7"); err == nil {
		t.Fatal("a non-artifact registry host must be rejected")
	}
}

// TestEnforceImageDisabled: with no registry host configured the guard is off
// and every image is accepted unchanged (pre-enforcement behaviour).
func TestEnforceImageDisabled(t *testing.T) {
	s := &Service{sandboxOrg: "sandbox"}
	for _, in := range []string{"docker.io/whatever:1", "sandbox/x:1", "plain"} {
		got, err := s.enforceSandboxImage(in)
		if err != nil || got != in {
			t.Fatalf("guard disabled: enforceSandboxImage(%q) = %q err=%v, want unchanged", in, got, err)
		}
	}
}

// TestSplitImageRef pins the host/remainder split used by the guard.
func TestSplitImageRef(t *testing.T) {
	cases := []struct{ ref, host, rest string }{
		{"git.agent.svc.cluster.local/sandbox/x:1", "git.agent.svc.cluster.local", "sandbox/x:1"},
		{"sandbox/x:1", "", "sandbox/x:1"},
		{"redis:7", "", "redis:7"},
		{"localhost/x", "localhost", "x"},
		{"http://host:5000/a/b:1", "host:5000", "a/b:1"},
	}
	for _, c := range cases {
		h, r := splitImageRef(c.ref)
		if h != c.host || r != c.rest {
			t.Errorf("splitImageRef(%q) = (%q,%q), want (%q,%q)", c.ref, h, r, c.host, c.rest)
		}
	}
}
