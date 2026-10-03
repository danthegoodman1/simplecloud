package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for range 3 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		s.Close()
	}
}

// Allocation must be dense and deterministic, and must reuse a freed range rather
// than drifting upward, or a long-lived install exhausts the space.
func TestProjectAllocationIsDenseAndReusesFreedRanges(t *testing.T) {
	s := open(t)
	a, err := s.CreateProject("a", t.TempDir(), "aws-us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateProject("b", t.TempDir(), "aws-us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.OverlayCIDR != "10.88.1.0/24" || b.OverlayCIDR != "10.88.2.0/24" {
		t.Errorf("want dense /24s, got %s and %s", a.OverlayCIDR, b.OverlayCIDR)
	}
	if a.HubListenPort != FirstListenPort || b.HubListenPort != FirstListenPort+1 {
		t.Errorf("want dense listen ports, got %d and %d", a.HubListenPort, b.HubListenPort)
	}
	if a.HubOverlayIP() != "10.88.1.1" {
		t.Errorf("hub overlay IP: %s", a.HubOverlayIP())
	}
	if a.InterfaceName() != "wg-"+a.ID {
		t.Errorf("interface name: %s", a.InterfaceName())
	}
	if err := s.DeleteProject(a.ID); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateProject("c", t.TempDir(), "aws-us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.OverlayCIDR != "10.88.1.0/24" || c.HubListenPort != FirstListenPort {
		t.Errorf("a freed range should be reused, got %s port %d", c.OverlayCIDR, c.HubListenPort)
	}
}

func TestOneProjectPerDirectory(t *testing.T) {
	s := open(t)
	dir := t.TempDir()
	if _, err := s.CreateProject("a", dir, "aws-us-east-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject("b", dir, "aws-us-east-1"); err == nil {
		t.Error("a second project in the same directory should be rejected")
	}
}

// Resolving upward from a subdirectory is what lets commands run anywhere inside
// a project, the way git does.
func TestProjectForDirWalksUpAndRebinds(t *testing.T) {
	s := open(t)
	dir := t.TempDir()
	p, err := s.CreateProject("app", dir, "aws-us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(dir, "a", "b")
	got, err := s.ProjectForDir(deep)
	if err != nil {
		t.Fatalf("resolve from subdirectory: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("resolved the wrong project")
	}
	if _, err := s.ProjectForDir(t.TempDir()); err != ErrNoProject {
		t.Errorf("want ErrNoProject, got %v", err)
	}
	moved := t.TempDir()
	if err := s.RebindProject(p.ID, moved); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ProjectForDir(moved); err != nil || got.ID != p.ID {
		t.Errorf("rebind did not take effect: %v", err)
	}
}

// A slot keeps its key and addresses for life, so replacing a sandbox never
// rewrites the hub's peer set.
func TestSlotAllocationIsStableAcrossCalls(t *testing.T) {
	s := open(t)
	p, _ := s.CreateProject("app", t.TempDir(), "aws-us-east-1")
	first, err := s.AllocateSlot(p, "web", 1, "priv", "pub", 48080)
	if err != nil {
		t.Fatal(err)
	}
	if first.OverlayIP != "10.88.1.10" || first.LoopbackIP != "127.0.1.10" {
		t.Errorf("first slot addresses: %s %s", first.OverlayIP, first.LoopbackIP)
	}
	again, err := s.AllocateSlot(p, "web", 1, "other-priv", "other-pub", 48080)
	if err != nil {
		t.Fatal(err)
	}
	if again.OverlayIP != first.OverlayIP || again.WGPrivateKey != "priv" {
		t.Errorf("re-allocating a slot must return the existing one, got %+v", again)
	}
	second, _ := s.AllocateSlot(p, "db", 1, "p2", "u2", 48080)
	if second.OverlayIP != "10.88.1.11" || second.LoopbackIP != "127.0.1.11" {
		t.Errorf("second slot addresses: %s %s", second.OverlayIP, second.LoopbackIP)
	}
	// A freed slot's addresses come back into use.
	if err := s.DeleteSlot(p.ID, "web-1"); err != nil {
		t.Fatal(err)
	}
	third, _ := s.AllocateSlot(p, "cache", 1, "p3", "u3", 48080)
	if third.OverlayIP != "10.88.1.10" {
		t.Errorf("a freed overlay address should be reused, got %s", third.OverlayIP)
	}
}

func TestSlotRoundTrip(t *testing.T) {
	s := open(t)
	p, _ := s.CreateProject("app", t.TempDir(), "aws-us-east-1")
	slot, _ := s.AllocateSlot(p, "web", 1, "priv", "pub", 48080)
	slot.SandboxID = "sb-1"
	slot.SandboxStatus = "running"
	slot.DoorbellHost = "48080-x.archil.app"
	slot.DoorbellToken = "secret"
	slot.LogCursor = 4096
	if err := s.PutSlot(slot); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSlot(p.ID, "web-1")
	if err != nil || got == nil {
		t.Fatalf("get slot: %v", err)
	}
	if got.SandboxID != "sb-1" || got.LogCursor != 4096 || got.DoorbellToken != "secret" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	// A slot's identity must survive a sandbox swap untouched.
	if got.WGPrivateKey != "priv" || got.OverlayIP != slot.OverlayIP {
		t.Errorf("slot identity changed: %+v", got)
	}
	if missing, err := s.GetSlot(p.ID, "nope-1"); err != nil || missing != nil {
		t.Errorf("want nil for a missing slot, got %v %v", missing, err)
	}
}

func TestServicesVolumesExposuresRoundTrip(t *testing.T) {
	s := open(t)
	p, _ := s.CreateProject("app", t.TempDir(), "aws-us-east-1")
	if err := s.PutService(&Service{ProjectID: p.ID, Name: "web", Image: "nginx", ConfigHash: "h1", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	// Writing again must update rather than fail, so deploys are re-runnable.
	if err := s.PutService(&Service{ProjectID: p.ID, Name: "web", Image: "nginx:2", ConfigHash: "h2", Replicas: 3}); err != nil {
		t.Fatal(err)
	}
	svc, _ := s.GetService(p.ID, "web")
	if svc.Image != "nginx:2" || svc.ConfigHash != "h2" || svc.Replicas != 3 {
		t.Errorf("service not updated: %+v", svc)
	}
	if err := s.PutVolume(&Volume{ProjectID: p.ID, Name: "data", DiskID: "dsk-1", MountPath: "/var/lib", Kind: "volume"}); err != nil {
		t.Fatal(err)
	}
	vols, _ := s.ListVolumes(p.ID)
	if len(vols) != 1 || vols[0].DiskID != "dsk-1" {
		t.Errorf("volumes: %+v", vols)
	}
	if err := s.PutExposure(&Exposure{ProjectID: p.ID, Slot: "web-1", Port: 8080, Hostname: "h"}); err != nil {
		t.Fatal(err)
	}
	exp, _ := s.ListExposures(p.ID)
	if len(exp) != 1 || exp[0].Hostname != "h" {
		t.Errorf("exposures: %+v", exp)
	}
	// Deleting a project must take its children with it.
	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatal(err)
	}
	if svcs, _ := s.ListServices(p.ID); len(svcs) != 0 {
		t.Errorf("services survived project deletion: %+v", svcs)
	}
	if vols, _ := s.ListVolumes(p.ID); len(vols) != 0 {
		t.Errorf("volumes survived project deletion: %+v", vols)
	}
}

func TestHubRoundTrip(t *testing.T) {
	s := open(t)
	if h, err := s.GetHub(); err != nil || h != nil {
		t.Errorf("want no hub initially, got %v %v", h, err)
	}
	if err := s.PutHub(&Hub{SSHTarget: "root@1.2.3.4", Endpoint: "1.2.3.4", HostKey: "ssh-ed25519 AAA", Bootstrapped: true}); err != nil {
		t.Fatal(err)
	}
	h, err := s.GetHub()
	if err != nil || h == nil || !h.Bootstrapped || h.HostKey != "ssh-ed25519 AAA" {
		t.Fatalf("hub round trip: %+v %v", h, err)
	}
	// Repointing the hub must not require clearing state.
	if err := s.PutHub(&Hub{SSHTarget: "root@5.6.7.8", Endpoint: "5.6.7.8"}); err != nil {
		t.Fatal(err)
	}
	h, _ = s.GetHub()
	if h.SSHTarget != "root@5.6.7.8" {
		t.Errorf("hub not updated: %+v", h)
	}
}

func TestOverlayExhaustionReportsClearly(t *testing.T) {
	s := open(t)
	p, _ := s.CreateProject("app", t.TempDir(), "aws-us-east-1")
	// Fill every slot address in the project's range.
	for i := FirstSlotOctet; i <= LastSlotOctet; i++ {
		if _, err := s.AllocateSlot(p, "svc", i, "p", "u", 48080); err != nil {
			t.Fatalf("allocating slot %d: %v", i, err)
		}
	}
	_, err := s.AllocateSlot(p, "svc", 9999, "p", "u", 48080)
	if err == nil || !strings.Contains(err.Error(), "no free address") {
		t.Errorf("want a clear exhaustion error, got %v", err)
	}
	// The loopback alias is derived from the overlay octet, so the two allocations
	// can never disagree about how much space is left.
	slot, err := s.GetSlot(p.ID, "svc-10")
	if err != nil || slot == nil {
		t.Fatalf("get slot: %v", err)
	}
	if slot.LoopbackIP != LoopbackPrefix+".10" {
		t.Errorf("loopback should track the overlay octet, got %s", slot.LoopbackIP)
	}
}
