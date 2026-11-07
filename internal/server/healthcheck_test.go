package server

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestGatewayHealthcheckSubcommand(t *testing.T) {
	// Build temporary zcode-gateway binary to test the CLI command
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "test-zcode-gateway.exe")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/zcode-gateway")
	buildCmd.Dir = filepath.Join("..", "..")
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, string(out))
	}

	// 1. Healthcheck fails when no server is listening
	freeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := freeLn.Addr().(*net.TCPAddr).Port
	_ = freeLn.Close()

	cmdFail := exec.Command(binPath, "healthcheck")
	cmdFail.Env = append(os.Environ(), "PORT="+strconv.Itoa(freePort), "BIND_HOST=127.0.0.1")
	failOut, failErr := cmdFail.CombinedOutput()
	if failErr == nil {
		t.Fatalf("expected healthcheck to fail on closed port %d, got success: %s", freePort, string(failOut))
	}

	// 2. Healthcheck succeeds (exit code 0) when gateway is serving
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srvPort := srvLn.Addr().(*net.TCPAddr).Port
	Configure(Config{Port: srvPort, Host: "127.0.0.1", BridgeToken: "test-token"})
	go func() {
		_ = Serve(ctx, srvLn)
	}()
	time.Sleep(30 * time.Millisecond)

	cmdSuccess := exec.Command(binPath, "healthcheck")
	cmdSuccess.Env = append(os.Environ(), "PORT="+strconv.Itoa(srvPort), "BIND_HOST=127.0.0.1")
	succOut, succErr := cmdSuccess.CombinedOutput()
	if succErr != nil {
		t.Fatalf("expected healthcheck to succeed on active server: %v, out: %s", succErr, string(succOut))
	}
}
