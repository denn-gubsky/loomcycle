package main

import "testing"

func TestLoadConfig_RequiresImage(t *testing.T) {
	t.Setenv("SANDBOX_IMAGE", "")
	t.Setenv("SANDBOX_AUTH_TOKEN", "tok")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when SANDBOX_IMAGE unset")
	}
}

func TestLoadConfig_RequiresAuthUnlessAnon(t *testing.T) {
	t.Setenv("SANDBOX_IMAGE", "img")
	t.Setenv("SANDBOX_AUTH_TOKEN", "")
	t.Setenv("SANDBOX_ALLOW_ANON", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when no token and no anon")
	}
	t.Setenv("SANDBOX_ALLOW_ANON", "1")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("anon mode should be allowed: %v", err)
	}
}

func TestLoadConfig_RejectsUnknownRuntime(t *testing.T) {
	t.Setenv("SANDBOX_IMAGE", "img")
	t.Setenv("SANDBOX_AUTH_TOKEN", "tok")
	t.Setenv("SANDBOX_RUNTIME", "bogus")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error for unknown runtime")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("SANDBOX_IMAGE", "img")
	t.Setenv("SANDBOX_AUTH_TOKEN", "tok")
	t.Setenv("SANDBOX_RUNTIME", "")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":9000" || c.MaxTmpfsMB != 2048 || c.MaxSessions != 32 {
		t.Errorf("defaults wrong: %+v", c)
	}
	// Per-session defaults equal the ceilings unless separately narrowed.
	if c.DefCPUs != c.MaxCPUs || c.DefMemMB != c.MaxMemMB || c.DefPids != c.MaxPids {
		t.Errorf("defaults should equal ceilings: %+v", c)
	}
	if c.CtrUser != "1000:1000" {
		t.Errorf("container user default should be non-root: %q", c.CtrUser)
	}
}
