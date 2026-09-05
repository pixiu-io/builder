package runtime

import "testing"

func TestNormalize(t *testing.T) {
	got, err := Normalize("")
	if err != nil || got != Containerd {
		t.Fatalf("empty → containerd, got %q err=%v", got, err)
	}
	got, err = Normalize("Docker")
	if err != nil || got != Docker {
		t.Fatalf("Docker → docker, got %q err=%v", got, err)
	}
	if _, err := Normalize("podman"); err == nil {
		t.Fatal("podman should error")
	}
}

func TestResolve(t *testing.T) {
	got, err := Resolve("", "")
	if err != nil || got != Containerd {
		t.Fatalf("default containerd, got %q", got)
	}
	got, err = Resolve("", "/tmp/fake-docker")
	if err != nil || got != Docker {
		t.Fatalf("custom dockerBin → docker for tests, got %q", got)
	}
	got, err = Resolve("containerd", "/tmp/fake-docker")
	if err != nil || got != Containerd {
		t.Fatalf("explicit wins, got %q", got)
	}
}

func TestParseBind(t *testing.T) {
	src, dst, opt, err := parseBind("/a:/b:ro")
	if err != nil || src != "/a" || dst != "/b" || opt != "rbind:ro" {
		t.Fatalf("got %s %s %s err=%v", src, dst, opt, err)
	}
}
