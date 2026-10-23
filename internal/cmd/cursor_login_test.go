package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

// The cursor authenticator must be registered on the CLI manager, or
// `--cursor-login` fails before reaching cursor-agent.
func TestAuthManagerRegistersCursorAuthenticator(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub requires a POSIX shell")
	}
	binDir := t.TempDir()
	stub := "#!/bin/sh\nexit 3\n"
	if err := os.WriteFile(filepath.Join(binDir, "cursor-agent"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	_, _, err := newAuthManager().Login(context.Background(), "cursor", &config.Config{}, &sdkAuth.LoginOptions{})
	if err == nil {
		t.Fatal("expected login to fail against the failing stub")
	}
	if strings.Contains(err.Error(), "not registered") {
		t.Fatalf("cursor authenticator is not registered: %v", err)
	}
	if !strings.Contains(err.Error(), "cursor-agent login failed") {
		t.Fatalf("Login() error = %v, want the cursor-agent login failure", err)
	}
}
