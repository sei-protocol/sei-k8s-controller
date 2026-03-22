package v1alpha1

// MonitoringConfig controls observability resources.
type MonitoringConfig struct {
	// ServiceMonitor creates a monitoring.coreos.com/v1 ServiceMonitor.
	// Presence (non-nil) enables it; set to nil to disable.
	// +optional
	ServiceMonitor *ServiceMonitorConfig `json:"serviceMonitor,omitempty"`
}

// ServiceMonitorConfig defines the ServiceMonitor.
type ServiceMonitorConfig struct {
	// Interval is the Prometheus scrape interval.
	// +optional
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Pattern="^[0-9]+(ms|s|m|h)$"
	Interval string `json:"interval,omitempty"`

	// Labels are added to the ServiceMonitor metadata.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}
