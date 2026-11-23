package main

import (
	"testing"
)

func TestWorkspaceContainerOptions_ReadsTheSettingsAndRefusesMalformedOnes(t *testing.T) {
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE", "img:1")
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_RUNTIME", "runsc")
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_MEMORY_MB", "512")
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_CPUS", "1.5")
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_PIDS", "64")
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_TMP_MB", "32")
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_HOST_ROOT", "/srv/workspaces")
	o, err := workspaceContainerOptions()
	if err != nil {
		t.Fatal(err)
	}
	if o.Image != "img:1" || o.Runtime != "runsc" || o.MemoryMB != 512 || o.CPUs != 1.5 || o.Pids != 64 || o.TmpMB != 32 || o.HostRoot != "/srv/workspaces" {
		t.Errorf("options = %+v", o)
	}
	for k, v := range map[string]string{
		"LOOMCYCLE_WORKSPACE_CONTAINER_MEMORY_MB": "8",
		"LOOMCYCLE_WORKSPACE_CONTAINER_CPUS":      "lots",
		"LOOMCYCLE_WORKSPACE_CONTAINER_PIDS":      "-1",
		"LOOMCYCLE_WORKSPACE_CONTAINER_HOST_ROOT": "relative/root",
	} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := workspaceContainerOptions(); err == nil {
				t.Errorf("%s=%s accepted", k, v)
			}
		})
	}
}

func TestWorkspaceContainerOptions_EgressNeedsItsDirectory(t *testing.T) {
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE", "img:1")
	o, err := workspaceContainerOptions()
	if err != nil || o.Egress != nil {
		t.Fatalf("without a directory: egress %+v, err %v", o.Egress, err)
	}
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR", "/run/lce")
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_HOST_DIR", "/host/lce")
	o, err = workspaceContainerOptions()
	if err != nil || o.Egress == nil || o.Egress.Dir != "/run/lce" || o.Egress.HostDir != "/host/lce" {
		t.Fatalf("egress %+v, err %v", o.Egress, err)
	}
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR", "relative")
	if _, err := workspaceContainerOptions(); err == nil {
		t.Error("a relative egress directory accepted")
	}
	t.Setenv("LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR", "")
	if _, err := workspaceContainerOptions(); err == nil {
		t.Error("an egress host directory without the directory accepted")
	}
}
