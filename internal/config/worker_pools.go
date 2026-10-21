package config

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

type WorkerPool struct {
	Name            string        `kdl:",arg"`
	Provider        string        `kdl:"provider"`
	Command         []string      `kdl:"command"`
	ResourceID      string        `kdl:"resource-id"`
	Profiles        []string      `kdl:"profiles"`
	ImageProfiles   []string      `kdl:"image-profiles"`
	ServiceProfiles []string      `kdl:"service-profiles"`
	Labels          []string      `kdl:"labels"`
	CPUs            string        `kdl:"cpus"`
	Memory          string        `kdl:"memory"`
	Slots           int           `kdl:"slots"`
	UrgentReserve   int           `kdl:"urgent-reserve"`
	WarmNodes       int           `kdl:"warm-nodes"`
	MaxNodes        int           `kdl:"max-nodes"`
	IdleTimeout     time.Duration `kdl:"idle-timeout,format:units"`
	StartupTimeout  time.Duration `kdl:"startup-timeout,format:units"`
	PricePerHour    float64       `kdl:"price-per-hour"`
	MaxHourlyCost   float64       `kdl:"max-hourly-cost"`
}

var vmssResourceID = regexp.MustCompile(`(?i)^/subscriptions/[^/\s?#]+/resourceGroups/[^/\s?#]+/providers/Microsoft\.Compute/virtualMachineScaleSets/[^/\s?#]+$`)

func (p WorkerPool) validate() error {
	if p.Name == "" || strings.ContainsAny(p.Name, "/\\\x00") {
		return fmt.Errorf("name is required")
	}
	switch p.Provider {
	case "command":
		if len(p.Command) == 0 || p.Command[0] == "" || p.ResourceID != "" {
			return fmt.Errorf("command provider requires command and no resource-id")
		}
	case "azure-vmss":
		if !vmssResourceID.MatchString(p.ResourceID) {
			return fmt.Errorf("azure-vmss requires a scale set resource-id")
		}
	default:
		return fmt.Errorf("provider must be command or azure-vmss")
	}
	if len(p.Profiles) == 0 || p.CPUs == "" || p.Memory == "" {
		return fmt.Errorf("profiles, cpus and memory are required")
	}
	if _, _, err := ResourceValues(p.CPUs, p.Memory); err != nil {
		return err
	}
	if p.Slots < 1 || p.Slots > 4096 || p.UrgentReserve < 0 || p.UrgentReserve >= p.Slots || p.MaxNodes < 1 || p.MaxNodes > 10000 || p.WarmNodes < 0 || p.WarmNodes > p.MaxNodes {
		return fmt.Errorf("invalid capacity, reserve or node bounds")
	}
	if p.IdleTimeout < time.Second || p.IdleTimeout > 24*time.Hour || p.StartupTimeout < time.Second || p.StartupTimeout > 24*time.Hour {
		return fmt.Errorf("idle-timeout and startup-timeout must be 1s..24h")
	}
	if math.IsNaN(p.PricePerHour) || math.IsInf(p.PricePerHour, 0) || p.PricePerHour < 0 || math.IsNaN(p.MaxHourlyCost) || math.IsInf(p.MaxHourlyCost, 0) || p.MaxHourlyCost < 0 {
		return fmt.Errorf("invalid hourly cost")
	}
	if p.MaxHourlyCost > 0 && (p.PricePerHour <= 0 || float64(p.WarmNodes)*p.PricePerHour > p.MaxHourlyCost+1e-9) {
		return fmt.Errorf("cost cap requires price-per-hour and must cover warm-nodes")
	}
	return nil
}
