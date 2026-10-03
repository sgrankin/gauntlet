package config

import (
	"fmt"
	"time"
)

type CircuitBreaker struct {
	Threshold  int           `kdl:"threshold"`
	Window     time.Duration `kdl:"window,format:units"`
	Backoff    time.Duration `kdl:"backoff,format:units"`
	MaxBackoff time.Duration `kdl:"max-backoff,format:units"`
}

func (c *CircuitBreaker) defaults() {
	if c.Threshold == 0 {
		c.Threshold = 3
	}
	if c.Window == 0 {
		c.Window = 5 * time.Minute
	}
	if c.Backoff == 0 {
		c.Backoff = 30 * time.Second
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 10 * time.Minute
	}
}
func (c *CircuitBreaker) validate() error {
	if c.Threshold < 2 || c.Threshold > 100 || c.Window <= 0 || c.Backoff <= 0 || c.MaxBackoff < c.Backoff || c.MaxBackoff > time.Hour {
		return fmt.Errorf("circuit-breaker: invalid threshold, window, or backoff")
	}
	return nil
}
