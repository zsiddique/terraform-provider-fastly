package fastly

import (
	"fmt"
	"log"

	gofastly "github.com/fastly/go-fastly/v3/fastly"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
)

type KafkaServiceAttributeHandler struct {
	*DefaultServiceAttributeHandler
}

func NewServiceLoggingKafka(sa ServiceMetadata) ServiceAttributeDefinition {
	return &KafkaServiceAttributeHandler{
		&DefaultServiceAttributeHandler{
			key:             "logging_kafka",
			serviceMetadata: sa,
		},
	}
}

func (h *KafkaServiceAttributeHandler) Register(s *schema.Resource) error {
	var blockAttributes = map[string]*schema.Schema{
		// Required fields
		"name": {
			Type:        schema.TypeString,
			Required:    true,
			Description: "The unique name of the Kafka logging endpoint. It is important to note that changing this attribute will delete and recreate the resource",
		},

		"topic": {
			Type:        schema.TypeString,
			Required:    true,
			Description: "The Kafka topic to send logs to",
		},

		"brokers": {
			Type:        schema.TypeString,
			Required:    true,
			Description: "A comma-separated list of IP addresses or hostnames of Kafka brokers",
		},

		// Optional
		"compression_codec": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "The codec used for compression of your logs. One of: `gzip`, `snappy`, `lz4`",
		},

		"required_acks": {
			Type:     schema.TypeString,
			Optional: true,
			Description: "The Number of acknowledgements a leader must receive before a write is considered successful. One of: `1` (default) One server needs to respond. `0` No servers need to respond. `-1`	Wait for all in-sync replicas to respond",
		},

		"use_tls": {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Whether to use TLS for secure logging. Can be either `true` or `false`",
		},

		"tls_ca_cert": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "A secure certificate to authenticate the server with. Must be in PEM format",
			Sensitive:   true,
			// Related issue for weird behavior - https://github.com/hashicorp/terraform-plugin-sdk/issues/160
			StateFunc: trimSpaceStateFunc,
		},

		"tls_client_cert": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "The client certificate used to make authenticated requests. Must be in PEM format",
			Sensitive:   true,
			// Related issue for weird behavior - https://github.com/hashicorp/terraform-plugin-sdk/issues/160
			StateFunc: trimSpaceStateFunc,
		},

		"tls_client_key": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "The client private key used to make authenticated requests. Must be in PEM format",
			Sensitive:   true,
			// Related issue for weird behavior - https://github.com/hashicorp/terraform-plugin-sdk/issues/160
			StateFunc: trimSpaceStateFunc,
		},

		"tls_hostname": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "The hostname used to verify the server's certificate. It can either be the Common Name or a Subject Alternative Name (SAN)",
		},

		"parse_log_keyvals": {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Enables parsing of key=value tuples from the beginning of a logline, turning them into record headers",
		},

		"request_max_bytes": {
			Type:        schema.TypeInt,
			Optional:    true,
			Description: "Maximum size of log batch, if non-zero. Defaults to 0 for unbounded",
		},

		"auth_method": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "SASL authentication method. One of: plain, scram-sha-256, scram-sha-512",
		},

		"user": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "SASL User",
		},

		"password": {
			Type:        schema.TypeString,
			Optional:    true,
			Description: "SASL Pass",
		},
	}

	if h.GetServiceMetadata().serviceType == ServiceTypeVCL {
		blockAttributes["format"] = &schema.Schema{
			Type:        schema.TypeString,
			Optional:    true,
			Description: "Apache style log formatting.",
		}
		blockAttributes["format_version"] = &schema.Schema{
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      2,
			Description:  "The version of the custom logging format used for the configured endpoint. Can be either 1 or 2. (default: 2).",
			ValidateFunc: validateLoggingFormatVersion(),
		}
		blockAttributes["placement"] = &schema.Schema{
			Type:         schema.TypeString,
			Optional:     true,
			Description:  "Where in the generated VCL the logging call should be placed.",
			ValidateFunc: validateLoggingPlacement(),
		}
		blockAttributes["response_condition"] = &schema.Schema{
			Type:        schema.TypeString,
			Optional:    true,
			Description: "The name of an existing condition in the configured endpoint, or leave blank to always execute.",
		}
	}

	s.Schema[h.GetKey()] = &schema.Schema{
		Type:     schema.TypeSet,
		Optional: true,
		Elem: &schema.Resource{
			Schema: blockAttributes,
		},
	}
	return nil
}

func (h *KafkaServiceAttributeHandler) Process(d *schema.ResourceData, latestVersion int, conn *gofastly.Client) error {
	serviceID := d.Id()
	oldLogCfg, newLogCfg := d.GetChange(h.GetKey())

	if oldLogCfg == nil {
		oldLogCfg = new(schema.Set)
	}
	if newLogCfg == nil {
		newLogCfg = new(schema.Set)
	}

	oldSet := oldLogCfg.(*schema.Set)
	newSet := newLogCfg.(*schema.Set)

	setDiff := NewSetDiff(func(resource interface{}) (interface{}, error) {
		t, ok := resource.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resource failed to be type asserted: %+v", resource)
		}
		return t["name"], nil
	})

	diffResult, err := setDiff.Diff(oldSet, newSet)
	if err != nil {
		return err
	}

	// DELETE removed resources
	for _, resource := range diffResult.Deleted {
		resource := resource.(map[string]interface{})
		opts := h.buildDelete(resource, serviceID, latestVersion)

		log.Printf("[DEBUG] Fastly Kafka logging endpoint removal opts: %#v", opts)

		if err := deleteKafka(conn, opts); err != nil {
			return err
		}
	}

	// CREATE new resources
	for _, resource := range diffResult.Added {
		resource := resource.(map[string]interface{})

		// @HACK for a TF SDK Issue.
		//
		// This ensures that the required, `name`, field is present.
		//
		// If we have made it this far and `name` is not present, it is most-likely due
		// to a defunct diff as noted here - https://github.com/hashicorp/terraform-plugin-sdk/issues/160#issuecomment-522935697.
		//
		// This is caused by using a StateFunc in a nested TypeSet. While the StateFunc
		// properly handles setting state with the StateFunc, it returns extra entries
		// during state Gets, specifically `GetChange("logging_kafka")` in this case.
		if v, ok := resource["name"]; !ok || v.(string) == "" {
			continue
		}

		opts := h.buildCreate(resource, serviceID, latestVersion)

		log.Printf("[DEBUG] Fastly Kafka logging addition opts: %#v", opts)

		if err := createKafka(conn, opts); err != nil {
			return err
		}
	}

	// UPDATE modified resources
	//
	// NOTE: although the go-fastly API client enables updating of a resource by
	// its 'name' attribute, this isn't possible within terraform due to
	// constraints in the data model/schema of the resources not having a uid.
	for _, resource := range diffResult.Modified {
		resource := resource.(map[string]interface{})

		opts := gofastly.UpdateKafkaInput{
			ServiceID:      d.Id(),
			ServiceVersion: latestVersion,
			Name:           resource["name"].(string),
		}

		// only attempt to update attributes that have changed
		modified := setDiff.Filter(resource, oldSet)

		// NOTE: where we transition between interface{} we lose the ability to
		// infer the underlying type being either a uint vs an int. This
		// materializes as a panic (yay) and so it's only at runtime we discover
		// this and so we've updated the below code to convert the type asserted
		// int into a uint before passing the value to gofastly.Uint().
		if v, ok := modified["brokers"]; ok {
			opts.Brokers = gofastly.String(v.(string))
		}
		if v, ok := modified["topic"]; ok {
			opts.Topic = gofastly.String(v.(string))
		}
		if v, ok := modified["required_acks"]; ok {
			opts.RequiredACKs = gofastly.String(v.(string))
		}
		if v, ok := modified["use_tls"]; ok {
			opts.UseTLS = gofastly.CBool(v.(bool))
		}
		if v, ok := modified["compression_codec"]; ok {
			opts.CompressionCodec = gofastly.String(v.(string))
		}
		if v, ok := modified["format"]; ok {
			opts.Format = gofastly.String(v.(string))
		}
		if v, ok := modified["format_version"]; ok {
			opts.FormatVersion = gofastly.Uint(uint(v.(int)))
		}
		if v, ok := modified["response_condition"]; ok {
			opts.ResponseCondition = gofastly.String(v.(string))
		}
		if v, ok := modified["placement"]; ok {
			opts.Placement = gofastly.String(v.(string))
		}
		if v, ok := modified["tls_ca_cert"]; ok {
			opts.TLSCACert = gofastly.String(v.(string))
		}
		if v, ok := modified["tls_hostname"]; ok {
			opts.TLSHostname = gofastly.String(v.(string))
		}
		if v, ok := modified["tls_client_cert"]; ok {
			opts.TLSClientCert = gofastly.String(v.(string))
		}
		if v, ok := modified["tls_client_key"]; ok {
			opts.TLSClientKey = gofastly.String(v.(string))
		}
		if v, ok := modified["parse_log_keyvals"]; ok {
			opts.ParseLogKeyvals = gofastly.CBool(v.(bool))
		}
		if v, ok := modified["request_max_bytes"]; ok {
			opts.RequestMaxBytes = gofastly.Uint(uint(v.(int)))
		}
		if v, ok := modified["auth_method"]; ok {
			opts.AuthMethod = gofastly.String(v.(string))
		}
		if v, ok := modified["user"]; ok {
			opts.User = gofastly.String(v.(string))
		}
		if v, ok := modified["password"]; ok {
			opts.Password = gofastly.String(v.(string))
		}

		log.Printf("[DEBUG] Update Kafka Opts: %#v", opts)
		_, err := conn.UpdateKafka(&opts)
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *KafkaServiceAttributeHandler) Read(d *schema.ResourceData, s *gofastly.ServiceDetail, conn *gofastly.Client) error {
	// refresh Kafka
	log.Printf("[DEBUG] Refreshing Kafka logging endpoints for (%s)", d.Id())
	kafkaList, err := conn.ListKafkas(&gofastly.ListKafkasInput{
		ServiceID:      d.Id(),
		ServiceVersion: s.ActiveVersion.Number,
	})

	if err != nil {
		return fmt.Errorf("[ERR] Error looking up Kafka logging endpoints for (%s), version (%v): %s", d.Id(), s.ActiveVersion.Number, err)
	}

	kafkaLogList := flattenKafka(kafkaList)

	if err := d.Set(h.GetKey(), kafkaLogList); err != nil {
		log.Printf("[WARN] Error setting Kafka logging endpoints for (%s): %s", d.Id(), err)
	}

	return nil
}

func createKafka(conn *gofastly.Client, i *gofastly.CreateKafkaInput) error {
	_, err := conn.CreateKafka(i)
	return err
}

func deleteKafka(conn *gofastly.Client, i *gofastly.DeleteKafkaInput) error {
	err := conn.DeleteKafka(i)

	if errRes, ok := err.(*gofastly.HTTPError); ok {
		if errRes.StatusCode != 404 {
			return err
		}
	} else if err != nil {
		return err
	}

	return nil
}

func flattenKafka(kafkaList []*gofastly.Kafka) []map[string]interface{} {
	var flattened []map[string]interface{}
	for _, s := range kafkaList {
		// Convert logging to a map for saving to state.
		flatKafka := map[string]interface{}{
			"name":               s.Name,
			"topic":              s.Topic,
			"brokers":            s.Brokers,
			"compression_codec":  s.CompressionCodec,
			"required_acks":      s.RequiredACKs,
			"use_tls":            s.UseTLS,
			"tls_ca_cert":        s.TLSCACert,
			"tls_client_cert":    s.TLSClientCert,
			"tls_client_key":     s.TLSClientKey,
			"tls_hostname":       s.TLSHostname,
			"format":             s.Format,
			"format_version":     s.FormatVersion,
			"placement":          s.Placement,
			"response_condition": s.ResponseCondition,
			"parse_log_keyvals":  s.ParseLogKeyvals,
			"request_max_bytes":  s.RequestMaxBytes,
			"auth_method":        s.AuthMethod,
			"user":               s.User,
			"password":           s.Password,
		}

		// prune any empty values that come from the default string value in structs
		for k, v := range flatKafka {
			if v == "" {
				delete(flatKafka, k)
			}
		}

		flattened = append(flattened, flatKafka)
	}

	return flattened
}

func (h *KafkaServiceAttributeHandler) buildCreate(kafkaMap interface{}, serviceID string, serviceVersion int) *gofastly.CreateKafkaInput {
	df := kafkaMap.(map[string]interface{})

	var vla = h.getVCLLoggingAttributes(df)
	return &gofastly.CreateKafkaInput{
		ServiceID:         serviceID,
		ServiceVersion:    serviceVersion,
		Name:              df["name"].(string),
		Brokers:           df["brokers"].(string),
		Topic:             df["topic"].(string),
		RequiredACKs:      df["required_acks"].(string),
		UseTLS:            gofastly.Compatibool(df["use_tls"].(bool)),
		CompressionCodec:  df["compression_codec"].(string),
		TLSCACert:         df["tls_ca_cert"].(string),
		TLSClientCert:     df["tls_client_cert"].(string),
		TLSClientKey:      df["tls_client_key"].(string),
		TLSHostname:       df["tls_hostname"].(string),
		Format:            vla.format,
		FormatVersion:     uintOrDefault(vla.formatVersion),
		Placement:         vla.placement,
		ResponseCondition: vla.responseCondition,
		ParseLogKeyvals:   gofastly.Compatibool(df["parse_log_keyvals"].(bool)),
		RequestMaxBytes:   uint(df["request_max_bytes"].(int)),
		AuthMethod:        df["auth_method"].(string),
		User:              df["user"].(string),
		Password:          df["password"].(string),
	}
}

func (h *KafkaServiceAttributeHandler) buildDelete(kafkaMap interface{}, serviceID string, serviceVersion int) *gofastly.DeleteKafkaInput {
	df := kafkaMap.(map[string]interface{})

	return &gofastly.DeleteKafkaInput{
		ServiceID:      serviceID,
		ServiceVersion: serviceVersion,
		Name:           df["name"].(string),
	}
}
