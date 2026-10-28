package authz

import "testing"

// RFC FA P5: the device-login approval and a bearer's self-retire are governed
// as the caller's own-credential questions; the CLI's start and poll come
// before any principal, so they are not.
func TestHTTPSurface_DeviceLoginAndSelfRevoke(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         Surface
		governed     bool
	}{
		{"POST", "/v1/_auth/device/approve", instance("mint", "principal", "token:self"), true},
		{"POST", "/v1/_auth/device/info", instance("mint", "principal", "token:self"), true},
		{"POST", "/v1/_auth/token/revoke", instance("manage", "principal", "token:self"), true},
		{"PUT", "/v1/_auth/tenant-policies/acme", instance("manage", "runtime", "tenants"), true},
		{"GET", "/v1/_auth/tenant-policies", instance("read", "runtime", "tenants"), true},
		{"POST", "/v1/_auth/device", Surface{}, false},
		{"POST", "/v1/_auth/device/token", Surface{}, false},
	} {
		got, governed := HTTPSurface(tc.method, tc.path)
		if got != tc.want || governed != tc.governed {
			t.Errorf("%s %s = %+v %v, want %+v %v", tc.method, tc.path, got, governed, tc.want, tc.governed)
		}
	}
}

// Every user class may approve its own device login and retire its own
// bearer; the self grant never reaches another principal's tokens.
func TestBaselines_GrantOnlyTheSelfTokenQuestions(t *testing.T) {
	bl := Baselines()
	for _, name := range []string{BaselineTenantOperator, BaselineMember, BaselineIsolated} {
		e, err := Compile(Input{Subject: Subject{TenantID: "t", ID: "alice"}, Bindings: []Binding{{ACL: name, SubjectKind: SubjectTenant}},
			ACLs: map[string]ACLVersion{name: {ACL: bl[name]}}, Now: testNow})
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"mint", "manage"} {
			if got := ask(e, action, "principal", "token:self"); got != Allowed {
				t.Errorf("%s: %s principal:token:self = %v", name, action, got)
			}
			if got := ask(e, action, "principal", "token:bob"); got == Allowed {
				t.Errorf("%s: %s principal:token:bob allowed", name, action)
			}
		}
	}
}
