package cloud

import "strings"

// GenerateCloudInit generates the cloud-init user data script for worker VMs.
// The script installs osmedeus, sets up SSH access, and joins the worker to the master.
func GenerateCloudInit(redisURL, sshPublicKey string, setupCommands []string) string {
	var sb strings.Builder

	sb.WriteString(`#!/bin/bash
set -e

# Install osmedeus
curl -fsSL https://www.osmedeus.org/install.sh | bash

# Setup SSH keys
mkdir -p ~/.ssh
`)
	sb.WriteString(`echo "`)
	sb.WriteString(sshPublicKey)
	sb.WriteString(`" >> ~/.ssh/authorized_keys
chmod 700 ~/.ssh
chmod 600 ~/.ssh/authorized_keys
`)

	if redisURL != "" {
		sb.WriteString("\n# Join as worker\nosmedeus worker join --redis-url ")
		sb.WriteString(redisURL)
		sb.WriteString(" --get-public-ip\n")
	}

	if len(setupCommands) > 0 {
		sb.WriteString("\n# Custom setup commands\n")
		for _, cmd := range setupCommands {
			sb.WriteString(cmd)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
