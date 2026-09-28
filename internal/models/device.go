package models

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// PointDef represents a measurement point definition.
type PointDef struct {
	Name       string   `json:"name" yaml:"name"`
	DataType   string   `json:"data_type" yaml:"data_type"`
	Unit       string   `json:"unit" yaml:"unit"`
	Address    string   `json:"address" yaml:"address"`
	AccessMode string   `json:"access_mode" yaml:"access_mode"`
	Min        *float64 `json:"min,omitempty" yaml:"min,omitempty"`
	Max        *float64 `json:"max,omitempty" yaml:"max,omitempty"`
	Mode       string   `json:"mode,omitempty" yaml:"mode,omitempty"`
	Scale      *float64 `json:"scale,omitempty" yaml:"scale,omitempty"`
	Offset     *float64 `json:"offset,omitempty" yaml:"offset,omitempty"`
}

// ModbusConfig holds Modbus TCP connection configuration.
type ModbusConfig struct {
	Host    string  `json:"host" yaml:"host"`
	Port    int     `json:"port" yaml:"port"`
	SlaveID int     `json:"slave_id" yaml:"slave_id"`
	Timeout float64 `json:"timeout" yaml:"timeout"`
}

// SimulatorConfig holds simulator configuration.
type SimulatorConfig struct {
	Timeout float64 `json:"timeout" yaml:"timeout"`
}

// VideoDeviceConfig holds video device configuration.
type VideoDeviceConfig struct {
	PyGBSentryDeviceID string  `json:"pygbsentry_device_id" yaml:"pygbsentry_device_id"`
	ChannelID         string  `json:"channel_id" yaml:"channel_id"`
	Timeout           float64 `json:"timeout" yaml:"timeout"`
}

// DeviceCreate represents a create device request.
type DeviceCreate struct {
	DeviceID       string                 `json:"device_id"`
	Name           string                 `json:"name"`
	Protocol       string                 `json:"protocol"`
	Config         map[string]interface{} `json:"config"`
	Points         []PointDef             `json:"points"`
	CollectInterval int                   `json:"collect_interval"`
}

// DeviceUpdate represents an update device request.
type DeviceUpdate struct {
	Name            *string                `json:"name,omitempty"`
	Config          map[string]interface{} `json:"config,omitempty"`
	Points          *[]PointDef            `json:"points,omitempty"`
	CollectInterval *int                   `json:"collect_interval,omitempty"`
}

// DeviceWritePolicyUpdate represents a device write protection policy update.
type DeviceWritePolicyUpdate struct {
	WriteVerify     *bool     `json:"write_verify,omitempty"`
	WriteRateLimit  *int      `json:"write_rate_limit,omitempty"`
	WriteAudit      *bool     `json:"write_audit,omitempty"`
	WriteWhitelist  *[]string `json:"write_whitelist,omitempty"`
}

// DeviceResponse represents a device response.
type DeviceResponse struct {
	DeviceID       string                 `json:"device_id"`
	Name           string                 `json:"name"`
	Protocol       string                 `json:"protocol"`
	Status         string                 `json:"status"`
	Collecting     bool                   `json:"collecting"`
	Config         map[string]interface{} `json:"config"`
	Points         []PointDef             `json:"points"`
	CollectInterval int                   `json:"collect_interval"`
	CreatedBy      string                 `json:"created_by,omitempty"`
	CreatedAt      string                 `json:"created_at"`
	UpdatedAt      string                 `json:"updated_at"`
	Version        int                    `json:"version"`
}

// SimulatorCreate represents a create simulator device request.
type SimulatorCreate struct {
	DeviceID       string     `json:"device_id"`
	Name           string     `json:"name"`
	Points         []PointDef `json:"points"`
	CollectInterval int       `json:"collect_interval"`
}

// DiscoverRequest represents a device discovery request.
type DiscoverRequest struct {
	Protocol string                 `json:"protocol"`
	Config  map[string]interface{} `json:"config"`
}

// TemplateCreate represents a create template request.
type TemplateCreate struct {
	DeviceID    string `json:"device_id"`
	TemplateName string `json:"template_name"`
}

// TemplateResponse represents a device template response.
type TemplateResponse struct {
	Name           string                 `json:"name"`
	Protocol       string                 `json:"protocol"`
	ConfigTemplate map[string]interface{} `json:"config_template"`
	PointTemplates []PointDef             `json:"point_templates"`
	CreatedAt      string                 `json:"created_at"`
}

// CreateFromTemplateRequest represents a create-from-template request.
type CreateFromTemplateRequest struct {
	TemplateName    string                 `json:"template_name"`
	DeviceID        string                 `json:"device_id"`
	Name            string                 `json:"name"`
	Config          map[string]interface{} `json:"config,omitempty"`
	CollectInterval int                    `json:"collect_interval"`
}

// ExportDevicesRequest represents an export devices request.
type ExportDevicesRequest struct {
	DeviceIDs []string `json:"device_ids,omitempty"`
}

// ImportDevicesRequest represents an import devices request.
type ImportDevicesRequest struct {
	Data      []map[string]interface{} `json:"data"`
	Overwrite bool                     `json:"overwrite"`
	Atomic   bool                      `json:"atomic"`
}

// WritePointRequest represents a write point value request.
type WritePointRequest struct {
	Point string      `json:"point"`
	Value interface{} `json:"value"`
}

// PushDataPointValue represents a single data point value push.
type PushDataPointValue struct {
	Point    string      `json:"point"`
	Value    interface{} `json:"value"`
	Quality  string      `json:"quality,omitempty"`
	Timestamp string     `json:"timestamp,omitempty"`
}

// PushDeviceDataRequest represents a push device data request.
type PushDeviceDataRequest struct {
	DeviceID string               `json:"device_id"`
	Data     []PushDataPointValue `json:"data"`
}

// UnmarshalJSON accepts every push payload shape callers use:
//   - {"data":[{"point":"p","value":1,...}]} — this edition's array contract;
//   - {"data":{"p":{"value":1,"quality":"good","timestamp":"..."}}} — the Python
//     edition's dict contract that ProtoForge's HTTP push loop sends;
//   - {"data":{"p":1}} — flat scalar map, accepted for webhook-client convenience.
func (r *PushDeviceDataRequest) UnmarshalJSON(raw []byte) error {
	var wire struct {
		DeviceID string          `json:"device_id"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	r.DeviceID = wire.DeviceID
	if len(wire.Data) == 0 {
		return fmt.Errorf("data is required")
	}
	trimmed := bytes.TrimSpace(wire.Data)
	switch trimmed[0] {
	case '[':
		return json.Unmarshal(trimmed, &r.Data)
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		for name, v := range obj {
			var inner struct {
				Value     interface{} `json:"value"`
				Quality   string      `json:"quality"`
				Timestamp string      `json:"timestamp"`
			}
			if err := json.Unmarshal(v, &inner); err == nil && inner.Value != nil {
				r.Data = append(r.Data, PushDataPointValue{Point: name, Value: inner.Value, Quality: inner.Quality, Timestamp: inner.Timestamp})
				continue
			}
			var scalar interface{}
			if err := json.Unmarshal(v, &scalar); err != nil {
				return fmt.Errorf("invalid value for point %q", name)
			}
			r.Data = append(r.Data, PushDataPointValue{Point: name, Value: scalar})
		}
		return nil
	default:
		return fmt.Errorf("data must be an array or an object")
	}
}

// BatchDeviceIDs represents a batch request with device IDs.
type BatchDeviceIDs struct {
	DeviceIDs []string `json:"device_ids"`
}
