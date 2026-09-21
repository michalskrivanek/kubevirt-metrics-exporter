// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func GetProjectDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for _, suffix := range []string{"/test/e2e", "/test"} {
		if strings.HasSuffix(wd, suffix) {
			return strings.TrimSuffix(wd, suffix)
		}
	}
	return wd
}

func Kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func KubectlApply(relPath string) error {
	path := filepath.Join(GetProjectDir(), relPath)
	_, err := Kubectl("apply", "-f", path)
	return err
}

func KubectlDelete(relPath string) error {
	path := filepath.Join(GetProjectDir(), relPath)
	_, err := Kubectl("delete", "-f", path, "--ignore-not-found")
	return err
}

func WaitForDaemonSetReady(name, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := Kubectl("get", "daemonset", name, "-n", namespace,
			"-o", "jsonpath={.status.desiredNumberScheduled},{.status.numberReady}")
		if err == nil {
			parts := strings.Split(out, ",")
			if len(parts) == 2 && parts[0] != "" && parts[0] != "0" && parts[0] == parts[1] {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("daemonset %s/%s not ready within %v", namespace, name, timeout)
}

func WaitForPodRunning(name, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := Kubectl("get", "pod", name, "-n", namespace,
			"-o", "jsonpath={.status.phase}")
		if err == nil && out == "Running" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("pod %s/%s not running within %v", namespace, name, timeout)
}

func GetExporterPodName(namespace string) (string, error) {
	out, err := Kubectl("get", "pods", "-n", namespace,
		"-l", "app=kubevirt-metrics-exporter",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", fmt.Errorf("getting exporter pod: %w", err)
	}
	if out == "" {
		return "", fmt.Errorf("no exporter pod found in %s", namespace)
	}
	return out, nil
}

func PortForwardAndGet(namespace, podName string, path string) (string, error) {
	return PortForwardAndGetOnPort(namespace, podName, 8080, path)
}

func PortForwardAndGetOnPort(namespace, podName string, targetPort int, path string) (string, error) {
	localPort, err := freePort()
	if err != nil {
		return "", fmt.Errorf("finding free port: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", namespace, podName, fmt.Sprintf("%d:%d", localPort, targetPort))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting port-forward: %w", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	url := fmt.Sprintf("http://localhost:%d%s", localPort, path)

	var resp *http.Response
	for range 10 {
		time.Sleep(500 * time.Millisecond)
		resp, err = http.Get(url)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return string(body), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
