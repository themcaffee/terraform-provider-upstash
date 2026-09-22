package utils

import (
	"context"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// CredentialsRemovedDescription is appended to the description of every
// resource and data source whose credential attributes were removed.
const CredentialsRemovedDescription = "Credentials (passwords and REST tokens) are intentionally not exposed as attributes, so that they are never persisted to state. Retrieve them from the Upstash Console or API instead."

// RemoveCredentialsStateUpgrader returns a StateUpgrader from schema version 0,
// in which the given credential attributes were Computed outputs, to version 1,
// in which they no longer exist. It deletes them from prior state so that no
// credential material survives the upgrade. All other attributes are passed
// through unchanged.
func RemoveCredentialsStateUpgrader(current map[string]*schema.Schema, removed ...string) schema.StateUpgrader {
	v0 := make(map[string]*schema.Schema, len(current)+len(removed))
	for k, v := range current {
		v0[k] = v
	}
	for _, k := range removed {
		v0[k] = &schema.Schema{Type: schema.TypeString, Computed: true, Sensitive: true}
	}

	return schema.StateUpgrader{
		Version: 0,
		Type:    (&schema.Resource{Schema: v0}).CoreConfigSchema().ImpliedType(),
		Upgrade: func(ctx context.Context, rawState map[string]interface{}, meta interface{}) (map[string]interface{}, error) {
			for _, k := range removed {
				delete(rawState, k)
			}
			return rawState, nil
		},
	}
}
