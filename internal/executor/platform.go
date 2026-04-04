package executor

import (
	"os"
	"runtime"
	"strings"
)

// DetectDocker checks if running inside a Docker container
func DetectDocker() bool {
	// Method 1: Check for /.dockerenv file
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	// Method 2: Check /proc/1/cgroup for /docker/ (Linux only)
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/1/cgroup")
		if err == nil && strings.Contains(string(data), "/docker/") {
			return true
		}
	}

	return false
}

// DetectKubernetes checks if running inside a Kubernetes pod
func DetectKubernetes() bool {
	// Method 1: Check for Kubernetes service account directory
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount"); err == nil {
		return true
	}

	// Method 2: Check /proc/1/cgroup for kubepods (Linux only)
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/1/cgroup")
		if err == nil {
			content := string(data)
			if strings.Contains(content, "/kubepods/") || strings.Contains(content, "/kubelet/") {
				return true
			}
		}
	}

	return false
}

// DetectCloudProvider detects cloud providers via DMI information.
// Returns the provider name (e.g. "aws", "gcp"), "on-prem" if no provider
// is detected, or "unknown" if DMI information cannot be read at all.
func DetectCloudProvider() string {
	// DMI detection only works on Linux
	if runtime.GOOS != "linux" {
		return "on-prem"
	}

	dmiPaths := []string{
		"/sys/class/dmi/id/sys_vendor",
		"/sys/devices/virtual/dmi/id/bios_vendor",
		"/sys/class/dmi/id/product_name",
		"/sys/class/dmi/id/chassis_asset_tag",
	}

	anyReadable := false
	for _, path := range dmiPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		anyReadable = true
		value := strings.ToLower(strings.TrimSpace(string(data)))

		switch {
		case strings.Contains(value, "amazon"):
			return "aws"
		case strings.Contains(value, "google"):
			return "gcp"
		case strings.Contains(value, "microsoft"):
			return "azure"
		case strings.Contains(value, "digitalocean"):
			return "digitalocean"
		case strings.Contains(value, "akamai") || strings.Contains(value, "linode"):
			return "linode"
		case strings.Contains(value, "vultr"):
			return "vultr"
		case strings.Contains(value, "hetzner"):
			return "hetzner"
		case strings.Contains(value, "oraclecloud"):
			return "oracle"
		}
	}

	if !anyReadable {
		return "unknown"
	}

	return "on-prem"
}
