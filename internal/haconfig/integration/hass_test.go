package integration_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pdeslaur/hangar/internal/haconfig"
	"github.com/pdeslaur/hangar/internal/haconfig/integration"
)

// configuration is configuration.yaml as ha-config-sync expects it, plus a
// shell command that the test's automation runs to show it was reloaded.
const configuration = `homeassistant:
  name: test
  packages: !include_dir_named packages/

automation ui: !include automations.yaml
automation code: !include_dir_merge_list automations/
script ui: !include scripts.yaml
script code: !include_dir_merge_named scripts/
scene ui: !include scenes.yaml
scene code: !include_dir_merge_list scenes/

shell_command:
  touch_reloaded: touch /config/reloaded

config_sync:
`

// reloaded runs the shell command when config_sync_succeeded fires.
const reloaded = `- id: reloaded
  alias: Reloaded
  triggers:
    - trigger: event
      event_type: config_sync_succeeded
  actions:
    - action: shell_command.touch_reloaded
`

// TestHomeAssistant runs the integration in the Home Assistant image named by
// HOMEASSISTANT_IMAGE, with docker, and talks to it as ha-config-sync does.
func TestHomeAssistant(t *testing.T) {
	image := os.Getenv("HOMEASSISTANT_IMAGE")
	if image == "" {
		t.Skip("HOMEASSISTANT_IMAGE unset, like ghcr.io/home-assistant/home-assistant:stable")
	}
	// t.TempDir's paths can exceed a socket path's 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "hass")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	files := map[string]string{
		"configuration.yaml": configuration,
		"automations.yaml":   "[]\n",
		"scripts.yaml":       "{}\n",
		"scenes.yaml":        "[]\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name := range haconfig.Owned {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := integration.Install(dir); err != nil {
		t.Fatal(err)
	}

	// As its own user, as in the pod, so that the socket is ours.
	out, err := exec.Command("docker", "run", "--detach",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--volume", dir+":/config",
		image).Output()
	if err != nil {
		t.Fatalf("starting home assistant: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "100", id).CombinedOutput()
			t.Logf("home assistant's logs:\n%s", logs)
		}
		exec.Command("docker", "rm", "--force", id).Run()
	})

	ctx := context.Background()
	hass := haconfig.NewHass(bridge(t, id))
	var version string
	deadline := time.Now().Add(3 * time.Minute)
	for version, err = hass.Version(ctx); err != nil; version, err = hass.Version(ctx) {
		if time.Now().After(deadline) {
			t.Fatalf("config_sync never served: %v", err)
		}
		time.Sleep(time.Second)
	}
	t.Logf("home assistant %s", version)

	t.Run("check", func(t *testing.T) {
		if errs, err := hass.Check(ctx); err != nil || errs != "" {
			t.Fatalf("valid config: errors %q, %v", errs, err)
		}
		broken := filepath.Join(dir, "packages", "broken.yaml")
		if err := os.WriteFile(broken, []byte("light: [\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(broken)
		errs, err := hass.Check(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if errs == "" {
			t.Error("broken package: no errors")
		}
	})

	t.Run("reload", func(t *testing.T) {
		marker := filepath.Join(dir, "reloaded")
		// Before the reload, nothing listens for the event.
		if err := hass.Succeeded(ctx, haconfig.KindDeploy, "before"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the automation ran before it was reloaded")
		}

		if err := os.WriteFile(filepath.Join(dir, "automations", "reloaded.yaml"), []byte(reloaded), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := hass.Reload(ctx, []string{haconfig.ReloadAutomation}); err != nil {
			t.Fatal(err)
		}
		if err := hass.Succeeded(ctx, haconfig.KindDeploy, "after"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the reloaded automation never ran on config_sync_succeeded")
			}
			time.Sleep(200 * time.Millisecond)
		}

		if err := hass.Reload(ctx, []string{haconfig.ReloadScript, haconfig.ReloadScene, haconfig.ReloadAll}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		if err := hass.Reload(ctx, []string{"homeassistant"}); err == nil {
			t.Error("reloaded a domain it doesn't deploy")
		}
		if err := hass.Failed(ctx, "other", "sha", "message"); err == nil {
			t.Error("reported an unknown kind")
		}
		if err := hass.Failed(ctx, haconfig.KindSync, "sha", "Checks failed."); err != nil {
			t.Error(err)
		}
	})
}

// bridge serves a Unix socket on the host reaching the integration's in
// container id, one docker exec of socat per connection: docker on macOS
// doesn't carry a socket across a bind mount.
func bridge(t *testing.T, id string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bridge")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				cmd := exec.Command("docker", "exec", "-i", id,
					"socat", "-", "UNIX-CONNECT:/config/"+haconfig.StateDir+"/api.sock")
				cmd.Stdout = conn
				in, err := cmd.StdinPipe()
				if err != nil {
					return
				}
				if err := cmd.Start(); err != nil {
					return
				}
				go func() {
					io.Copy(in, conn)
					in.Close()
				}()
				// Closing conn, once socat exits, ends the copy.
				cmd.Wait()
			}()
		}
	}()
	return socket
}
