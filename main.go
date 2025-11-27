package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

const (
	// GCP Metadata Server
	metadataBase = "http://metadata.google.internal/computeMetadata/v1/"
	slackURL     = "https://v7uagcoglkqlufu7bah6luxjta0dsfht.lambda-url.us-east-2.on.aws" // Keeping your original URL
	gracePeriod  = 15 * time.Minute
	checkInterval = 5 * time.Second
	defaultTerminate = 24
)

// getMetadata fetches data from GCP metadata server.
// GCP requires the "Metadata-Flavor: Google" header.
func getMetadata(path string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest("GET", metadataBase+path, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Add("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata %s returned %d", path, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response failed: %w", err)
	}

	return string(body), nil
}

// terminateInstance deletes the VM using the Google Compute Engine API.
func terminateInstance(projectID, zone, instanceName string) error {
	ctx := context.Background()
	
	// Create Compute Service
	// Ensure the VM's Service Account has "Compute Instance Admin" role
	computeService, err := compute.NewService(ctx, option.WithScopes(compute.ComputeScope))
	if err != nil {
		return fmt.Errorf("failed to create compute service: %w", err)
	}

	// Create the delete call
	call := computeService.Instances.Delete(projectID, zone, instanceName)
	
	// Execute
	_, err = call.Do()
	if err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}
	
	return nil
}

func sendSlackMessage(message string) {
	payload := map[string]string{"message": message}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal Slack message: %v", err)
		return
	}

	resp, err := http.Post(slackURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Slack POST failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		log.Printf("Slack API returned non-2xx status: %d", resp.StatusCode)
	}
}

// checkSpotTermination checks if the GCP VM is being preempted.
// GCP provides a 30-second warning window.
func checkSpotTermination() (bool, error) {
	// Method 1: Check "preempted" flag (Returns "TRUE" if preempted)
	status, err := getMetadata("instance/preempted")
	if err != nil {
		return false, err
	}

	if strings.TrimSpace(status) == "TRUE" {
		return true, nil
	}

	// Method 2 (Optional but robust): Check maintenance-event
	// event, _ := getMetadata("instance/maintenance-event")
	// if event == "TERMINATE_ON_HOST_MAINTENANCE" { return true, nil }

	return false, nil
}

func main() {
	terminateAfterHours := defaultTerminate
	if val, err := strconv.Atoi(os.Getenv("TERMINATE_AFTER_HOURS")); err == nil {
		terminateAfterHours = val
	}
	log.Printf("Instance will terminate in %d hours", terminateAfterHours)

	// Fetch basic info
	instanceID, err := getMetadata("instance/id")
	if err != nil {
		log.Fatalf("Failed to get instance ID: %v", err)
	}

	// In GCP, instance/name is the Hostname/Resource Name
	name, err := getMetadata("instance/name")
	if err != nil {
		log.Printf("Failed to get instance name: %v", err)
		name = "unknown"
	}

	// Zone returns full path: "projects/123/zones/us-central1-a"
	fullZone, err := getMetadata("instance/zone")
	if err != nil {
		log.Fatalf("Failed to get zone: %v", err)
	}
	zone := path.Base(fullZone) // Extract just "us-central1-a"

	// Machine Type returns full path
	fullType, err := getMetadata("instance/machine-type")
	if err != nil {
		log.Fatalf("Failed to get machine type: %v", err)
	}
	machineType := path.Base(fullType)

	// Project ID is needed for the API call to delete itself
	projectID, err := getMetadata("project/project-id")
	if err != nil {
		log.Fatalf("Failed to get project ID: %v", err)
	}

	message := fmt.Sprintf("GCP Instance Launched\n"+
		"```\n"+
		"Name: %s\n"+
		"ID: %s\n"+
		"Zone: %s\n"+
		"Type: %s\n"+
		"Project: %s\n"+
		"Terminate after: %d hours\n"+
		"```\n",
		name, instanceID, zone, machineType, projectID, terminateAfterHours)

	sendSlackMessage(message)

	startTime := time.Now()
	terminateAfter := time.Duration(terminateAfterHours) * time.Hour

	for {
		uptime := time.Since(startTime)

		// 1. Check TTL (Self-Termination)
		if uptime > terminateAfter {
			sendSlackMessage(fmt.Sprintf("Instance `%s` in `%s` crossed uptime threshold. Will terminate in %v", name, zone, gracePeriod))
			log.Printf("Crossed uptime threshold. Terminating in %v", gracePeriod)
			time.Sleep(gracePeriod)

			if err := terminateInstance(projectID, zone, name); err != nil {
				log.Printf("Termination failed: %v", err)
			}
			break
		}

		// 2. Check Spot/Preemptible Interruption
		// GCP provides a 30-second warning via metadata
		isPreempted, err := checkSpotTermination()
		if err != nil {
			log.Printf("Spot termination check failed: %v", err)
		} else if isPreempted {
			sendSlackMessage(fmt.Sprintf("🚨 Instance `%s` in `%s` is being PREEMPTED by GCP", name, zone))
			// We break loop, but GCP will likely kill the VM forcefully in <30s
			break
		}

		timeLeft := terminateAfter - uptime
		log.Printf("Time left: %v", timeLeft.Truncate(time.Second))
		time.Sleep(checkInterval)
	}
}