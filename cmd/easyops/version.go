package main

import "os"

// appVersion is the deployed image tag (env APP_VERSION), surfaced in /healthz.
func appVersion() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return "dev"
}
