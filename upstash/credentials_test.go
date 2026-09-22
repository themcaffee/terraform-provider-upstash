package upstash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ctyjson "github.com/hashicorp/go-cty/cty/json"
	"github.com/hashicorp/go-cty/cty/msgpack"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/upstash/terraform-provider-upstash/v2/upstash/client"
)

// credentialAttributes are the attributes that earlier provider versions
// persisted to state. None of them may exist in any schema.
var credentialAttributes = map[string]bool{
	"password":             true,
	"rest_token":           true,
	"read_only_rest_token": true,
	"token":                true,
	"read_only_token":      true,
}

// sentinel marks every credential value served by the mock API, so that any
// leak into state can be found by substring search regardless of attribute name.
const sentinel = "SENTINEL-CREDENTIAL"

func TestProviderInternalValidate(t *testing.T) {
	if err := Provider().InternalValidate(); err != nil {
		t.Fatal(err)
	}
}

func TestNoCredentialAttributesInAnySchema(t *testing.T) {
	p := Provider()
	check := func(kind string, resources map[string]*schema.Resource) {
		for name, r := range resources {
			walkSchema(r.Schema, name, func(path string) {
				t.Errorf("%s %s exposes credential attribute %s", kind, name, path)
			})
		}
	}
	check("resource", p.ResourcesMap)
	check("data source", p.DataSourcesMap)
}

func walkSchema(s map[string]*schema.Schema, prefix string, found func(string)) {
	for k, v := range s {
		if credentialAttributes[k] {
			found(prefix + "." + k)
		}
		if r, ok := v.Elem.(*schema.Resource); ok {
			walkSchema(r.Schema, prefix+"."+k, found)
		}
	}
}

// v0States are representative states written by the provider before the
// credential attributes were removed.
var v0States = map[string]map[string]interface{}{
	"upstash_redis_database": {
		"id":                   "upstash-database-db1",
		"database_id":          "db1",
		"database_name":        "prod",
		"platform":             "aws",
		"region":               "global",
		"primary_region":       "us-east-1",
		"read_regions":         []interface{}{"us-west-1"},
		"ip_allowlist":         []interface{}{"10.0.0.0/8"},
		"endpoint":             "example.upstash.io",
		"port":                 6379,
		"password":             sentinel + "-password",
		"rest_token":           sentinel + "-rest",
		"read_only_rest_token": sentinel + "-ro-rest",
		"tls":                  true,
		"consistent":           false,
		"multizone":            false,
		"eviction":             true,
		"auto_scale":           false,
		"prod_pack":            false,
		"budget":               20,
		"creation_time":        1700000000,
		"database_type":        "Pay as You Go",
		"state":                "active",
		"user_email":           "ops@example.com",
	},
	"upstash_vector_index": {
		"id":                  "idx1",
		"customer_id":         "cust",
		"name":                "vectors",
		"similarity_function": "COSINE",
		"dimension_count":     256,
		"endpoint":            "vec.upstash.io",
		"token":               sentinel + "-token",
		"read_only_token":     sentinel + "-ro-token",
		"type":                "payg",
		"region":              "us-east-1",
		"reserved_price":      0,
	},
	"upstash_search": {
		"id":              "srch1",
		"customer_id":     "cust",
		"name":            "search",
		"endpoint":        "search.upstash.io",
		"token":           sentinel + "-token",
		"read_only_token": sentinel + "-ro-token",
		"type":            "payg",
		"region":          "us-east-1",
		"reserved_price":  0,
	},
}

func upgradeState(t *testing.T, typeName string, version int64, state map[string]interface{}) map[string]interface{} {
	t.Helper()
	p := Provider()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := schema.NewGRPCProviderServer(p).UpgradeResourceState(context.Background(), &tfprotov5.UpgradeResourceStateRequest{
		TypeName: typeName,
		Version:  version,
		RawState: &tfprotov5.RawState{JSON: raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		t.Fatalf("upgrade diagnostic: %s: %s", d.Summary, d.Detail)
	}
	ty := p.ResourcesMap[typeName].CoreConfigSchema().ImpliedType()
	val, err := msgpack.Unmarshal(resp.UpgradedState.MsgPack, ty)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ctyjson.Marshal(val, ty)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), sentinel) {
		t.Fatalf("upgraded %s state still contains credential material: %s", typeName, out)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpgradeFromV0StateRemovesCredentials(t *testing.T) {
	for typeName, v0 := range v0States {
		t.Run(typeName, func(t *testing.T) {
			if got := Provider().ResourcesMap[typeName].SchemaVersion; got != 1 {
				t.Fatalf("SchemaVersion = %d, want 1", got)
			}
			upgraded := upgradeState(t, typeName, 0, v0)
			// Round-trip through JSON so numbers compare as they are stored.
			var prior map[string]interface{}
			raw, _ := json.Marshal(v0)
			_ = json.Unmarshal(raw, &prior)
			for k, want := range prior {
				got, present := upgraded[k]
				if credentialAttributes[k] {
					if present {
						t.Errorf("credential attribute %s survived the upgrade", k)
					}
					continue
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("attribute %s changed across upgrade: got %v, want %v", k, got, want)
				}
			}

			// State already at version 1 passes through unchanged.
			again := upgradeState(t, typeName, 1, upgraded)
			if fmt.Sprint(again) != fmt.Sprint(upgraded) {
				t.Errorf("version 1 state changed on upgrade:\n got  %v\n want %v", again, upgraded)
			}
		})
	}
}

// mockAPI serves just enough of the Upstash Developer API for the Redis,
// Vector and Search resources. Every response carries credentials, as the real
// API does for a read-write key.
type mockAPI struct {
	mu            sync.Mutex
	objects       map[string]map[string]interface{}
	servedSecrets atomic.Int64
}

func (m *mockAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)

	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v2/redis/database":
		m.objects["/v2/redis/database/db1"] = map[string]interface{}{
			"database_id":          "db1",
			"database_name":        body["database_name"],
			"region":               "global",
			"port":                 6379,
			"creation_time":        1700000000,
			"endpoint":             "example.upstash.io",
			"password":             sentinel + "-password",
			"rest_token":           sentinel + "-rest",
			"read_only_rest_token": sentinel + "-ro-rest",
			"tls":                  body["tls"],
			"budget":               body["budget"],
			"database_type":        "Pay as You Go",
			"state":                "active",
		}
		path = "/v2/redis/database/db1"
	case r.Method == http.MethodPost && path == "/v2/vector/index":
		m.objects["/v2/vector/index/idx1"] = map[string]interface{}{
			"id":                  "idx1",
			"customer_id":         "cust",
			"name":                body["name"],
			"similarity_function": body["similarity_function"],
			"dimension_count":     body["dimension_count"],
			"region":              body["region"],
			"type":                "paid",
			"endpoint":            "vec.upstash.io",
			"token":               sentinel + "-token",
			"read_only_token":     sentinel + "-ro-token",
		}
		path = "/v2/vector/index/idx1"
	case r.Method == http.MethodPost && path == "/v2/search":
		m.objects["/v2/search/srch1"] = map[string]interface{}{
			"id":              "srch1",
			"customer_id":     "cust",
			"name":            body["name"],
			"region":          body["region"],
			"type":            "paid",
			"endpoint":        "search.upstash.io",
			"token":           sentinel + "-token",
			"read_only_token": sentinel + "-ro-token",
		}
		path = "/v2/search/srch1"
	case r.Method == http.MethodDelete:
		delete(m.objects, path)
		w.WriteHeader(http.StatusOK)
		return
	case r.Method != http.MethodGet:
		http.Error(w, "unexpected request "+r.Method+" "+path, http.StatusBadRequest)
		return
	}

	obj, ok := m.objects[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	m.servedSecrets.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(obj)
}

const credentialsTestConfig = `
provider "upstash" {
  email   = "test@example.com"
  api_key = "test"
}

resource "upstash_redis_database" "db" {
  database_name = "prod"
  platform      = "aws"
}

resource "upstash_vector_index" "idx" {
  name                = "vectors"
  similarity_function = "COSINE"
  dimension_count     = 256
  region              = "us-east-1"
  type                = "payg"
}

resource "upstash_search" "srch" {
  name   = "search"
  region = "us-east-1"
  type   = "payg"
}

data "upstash_redis_database_data" "db" {
  database_id = upstash_redis_database.db.database_id
}

data "upstash_vector_index_data" "idx" {
  id = upstash_vector_index.idx.id
}

data "upstash_search_data" "srch" {
  id = upstash_search.srch.id
}
`

func assertNoCredentialsInState(s *terraform.State) error {
	for name, rs := range s.RootModule().Resources {
		for k, v := range rs.Primary.Attributes {
			if credentialAttributes[k] || strings.Contains(v, sentinel) {
				return fmt.Errorf("%s: attribute %s holds credential material", name, k)
			}
		}
	}
	return nil
}

func assertNoCredentialsInImport(states []*terraform.InstanceState) error {
	for _, is := range states {
		for k, v := range is.Attributes {
			if credentialAttributes[k] || strings.Contains(v, sentinel) {
				return fmt.Errorf("%s: imported attribute %s holds credential material", is.ID, k)
			}
		}
	}
	return nil
}

// TestStateContainsNoCredentials runs the provider under a real Terraform or
// OpenTofu binary against a mock API that returns credentials, and asserts on
// the resulting state after create, refresh and import, for every affected
// resource and data source. Each apply step also asserts that the follow-up
// plan is empty. Only upstash_redis_database supports import; the Vector and
// Search resources have no importer.
func TestStateContainsNoCredentials(t *testing.T) {
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		for _, bin := range []string{"tofu", "terraform"} {
			if path, err := exec.LookPath(bin); err == nil {
				t.Setenv("TF_ACC_TERRAFORM_PATH", path)
				break
			}
		}
	}
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		t.Skip("no tofu or terraform binary found; set TF_ACC_TERRAFORM_PATH")
	}
	// OpenTofu resolves the unqualified provider to its own registry host, so
	// the test harness must reattach the in-process provider under that address.
	if strings.HasSuffix(os.Getenv("TF_ACC_TERRAFORM_PATH"), "tofu") && os.Getenv("TF_ACC_PROVIDER_HOST") == "" {
		t.Setenv("TF_ACC_PROVIDER_HOST", "registry.opentofu.org")
	}

	api := &mockAPI{objects: map[string]map[string]interface{}{}}
	server := httptest.NewServer(api)
	defer server.Close()

	previous := client.UPSTASH_API_ENDPOINT
	client.UPSTASH_API_ENDPOINT = server.URL
	defer func() { client.UPSTASH_API_ENDPOINT = previous }()

	resource.UnitTest(t, resource.TestCase{
		ProviderFactories: map[string]func() (*schema.Provider, error){
			"upstash": func() (*schema.Provider, error) { return Provider(), nil },
		},
		Steps: []resource.TestStep{
			{
				Config: credentialsTestConfig,
				Check: resource.ComposeTestCheckFunc(
					assertNoCredentialsInState,
					func(*terraform.State) error {
						if api.servedSecrets.Load() == 0 {
							return fmt.Errorf("mock API never served credentials; test is vacuous")
						}
						return nil
					},
				),
			},
			{
				RefreshState: true,
				Check:        assertNoCredentialsInState,
			},
			{
				Config:            credentialsTestConfig,
				ResourceName:      "upstash_redis_database.db",
				ImportState:       true,
				ImportStateId:     "db1",
				ImportStateVerify: true,
				ImportStateCheck:  assertNoCredentialsInImport,
			},
		},
	})
}
