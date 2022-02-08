// Copyright 2022 Edgecast Inc., Licensed under the terms of the Apache 2.0 license.
// See LICENSE file in project root for terms.

package rulesengine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"terraform-provider-ec/ec/api"
	"terraform-provider-ec/ec/helper"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

const emptyPolicyFormat string = "{\"@type\":\"policy-create\",\"name\":\"Terraform Placeholder - %s\",\"platform\":\"%s\",\"rules\":[{\"@type\":\"rule-create\",\"description\":\"Placeholder rule created by the Edgecast Terraform Provider\",\"matches\":[{\"features\":[{\"type\":\"feature.comment\",\"value\":\"Empty policy created on %s\"}],\"ordinal\":1,\"type\":\"match.always\"}],\"name\":\"Placeholder Rule\"}],\"state\":\"locked\"}"

func ResourceRulesEngineV4Policy() *schema.Resource {
	return &schema.Resource{
		CreateContext: ResourcePolicyCreate,
		ReadContext:   ResourcePolicyRead,
		UpdateContext: ResourcePolicyUpdate,
		DeleteContext: ResourcePolicyDelete,
		Schema: map[string]*schema.Schema{
			"customeruserid": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "User ID to impersonate. If using MCC credentials, this parameter will be ignored"},
			"portaltypeid": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "Portal Type ID to impersonate. If using MCC credentials, this parameter will be ignored."},
			"account_number": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "Account to impersonate. If using MCC credentials, this parameter will be ignored.",
			},
			"deploy_to": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "The desired environment for the provided policy. Valid values are `production` and `staging`",
				ValidateFunc: validation.StringInSlice(
					[]string{"production", "staging"},
					false),
			},
			"deploy_request_id": {
				Type:     schema.TypeString,
				Computed: true},
			"policy": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "A Rules Engine Policy in JSON format",
				StateFunc:   cleanPolicyForTerrafomState,
				ValidateFunc: validation.All(
					validation.StringIsNotWhiteSpace,
					validation.StringIsJSON,
				),
			},
		},
	}
}

// ResourcePolicyCreate - Create a new policy and deploy it to a target platform
func ResourcePolicyCreate(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
) diag.Diagnostics {
	policy := d.Get("policy").(string)

	// messy - needs improvement - unmarshalling json, modifying, then
	// marshalling back to string state must always be locked
	policyMap := make(map[string]interface{})
	json.Unmarshal([]byte(policy), &policyMap)
	policyMap["state"] = "locked"

	policyBytes, err := json.Marshal(policyMap)
	if err != nil {
		return diag.FromErr(err)
	}

	policy = string(policyBytes)

	err = addPolicy(policy, false, d, m)

	if err != nil {
		return diag.FromErr(err)
	}

	return ResourcePolicyRead(ctx, d, m)
}

// ResourcePolicyRead reads the current policy
func ResourcePolicyRead(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
) diag.Diagnostics {

	policy, err := getPolicy(m, d)

	if err != nil {
		d.SetId("")
		return diag.FromErr(err)
	}

	// set id to policy id from body
	d.SetId(policy["id"].(string))

	// Remove unneeded policy and rule  metadata - this metadata interferes with
	// terraform diffs
	cleanPolicy(policy)

	// convert to json
	jsonBytes, err := json.Marshal(policy)

	if err != nil {
		d.SetId("")
		return diag.FromErr(
			fmt.Errorf("error marshaling policy to json : %v", err))
	}

	policyAsString := string(jsonBytes)
	log.Printf(
		"[INFO] Successfully retrieved policy %s: %s",
		d.Id(),
		policyAsString)

	d.Set("policy", policyAsString)

	return diag.Diagnostics{}
}

// ResourcePolicyUpdate adds/deletes/updates rules in the current policy and
// deploys the modified policy to a target platform
func ResourcePolicyUpdate(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
) diag.Diagnostics {
	return ResourcePolicyCreate(ctx, d, m)
}

// ResourcePolicyDelete creates a new empty placeholder policy and deploys it to
// a target platform instead of actual deletion.
func ResourcePolicyDelete(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
) diag.Diagnostics {
	// We will retrieve a fresh copy of the policy to prevent
	// sending an empty policy to the wrong platform
	policy, err := getPolicy(m, d)

	if err != nil {
		return diag.FromErr(err)
	}

	// pull out platform from existing policy
	platform := policy["platform"].(string)

	// You can't actually delete policies, so we will instead create a
	// placeholder empty policy for the customer for the given platform and
	// policy type
	timestamp := time.Now().Format(time.RFC3339)
	emptyPolicyJSON := fmt.Sprintf(
		emptyPolicyFormat,
		timestamp,
		platform,
		timestamp)

	err = addPolicy(emptyPolicyJSON, true, d, m)

	if err != nil {
		return diag.FromErr(err)
	}

	return diag.Diagnostics{}
}

func cleanPolicy(policyMap map[string]interface{}) {
	delete(policyMap, "id")
	delete(policyMap, "@id")
	delete(policyMap, "@type")
	delete(policyMap, "policy_type")
	delete(policyMap, "state") // will always have the value "locked"
	delete(policyMap, "history")
	delete(policyMap, "created_at")
	delete(policyMap, "updated_at")

	rules := policyMap["rules"].([]interface{})
	ruleMaps := make([]map[string]interface{}, 0)

	for _, rule := range rules {
		ruleMap := rule.(map[string]interface{})
		delete(ruleMap, "id")
		delete(ruleMap, "@id")
		delete(ruleMap, "@type")
		delete(ruleMap, "ordinal")
		delete(ruleMap, "created_at")
		delete(ruleMap, "updated_at")

		if matches, ok := ruleMap["matches"].([]interface{}); ok {
			// replace with cleaned matches
			ruleMap["matches"] = cleanMatches(matches)
		}

		ruleMaps = append(ruleMaps, ruleMap)
	}

	// replace with cleaned rules
	policyMap["rules"] = ruleMaps
}

// recursive function to remove unneeded metadata from matches
func cleanMatches(matches []interface{}) []map[string]interface{} {
	cleanedMatches := make([]map[string]interface{}, 0)

	for _, match := range matches {
		cleanedMatch := match.(map[string]interface{})
		delete(cleanedMatch, "ordinal")
		standardizeMatchFeature(cleanedMatch)

		// recursively clean child matches
		if childMatches, ok := cleanedMatch["matches"].([]interface{}); ok {
			cleanedMatch["matches"] = cleanMatches(childMatches)
		}

		if features, ok := cleanedMatch["features"].([]interface{}); ok {
			cleanedFeatures := make([]map[string]interface{}, 0)

			for _, feature := range features {
				cleanedFeature := feature.(map[string]interface{})
				delete(cleanedFeature, "ordinal")
				standardizeMatchFeature(cleanedFeature)
				cleanedFeatures = append(cleanedFeatures, cleanedFeature)
			}

			cleanedMatch["features"] = cleanedFeatures
		}

		cleanedMatches = append(cleanedMatches, cleanedMatch)
	}

	return cleanedMatches
}

// change string arrays to space-separated strings and standardize keys to
// hyperion standard i.e. "-" -> "_"
func standardizeMatchFeature(matchFeatureMap map[string]interface{}) {
	for k, v := range matchFeatureMap {
		delete(matchFeatureMap, k)
		// the json library unmarshals all arrays into []interface{}
		// so we have to do this roundabout way of converting to []string
		if valArray, ok := v.([]interface{}); ok {
			if stringArray, ok := helper.ConvertSliceToStrings(valArray); ok {
				v = strings.Join(stringArray, " ")
			}
		}
		matchFeatureMap[strings.Replace(k, "-", "_", -1)] = v
	}
}

func getDeployRequestData(
	d *schema.ResourceData,
	policyID int,
) *api.AddDeployRequest {
	return &api.AddDeployRequest{
		Message:     "Auto-submitted policy",
		PolicyID:    policyID,
		Environment: d.Get("deploy_to").(string),
	}
}

func getPolicy(
	m interface{},
	d *schema.ResourceData,
) (map[string]interface{}, error) {
	config := m.(**api.ClientConfig)
	(*config).AccountNumber = d.Get("account_number").(string)
	// Portal Type IDs: 1=MCC 2=PCC 3=WCC 4=WCC 5=opencdn
	portalTypeID := d.Get("portaltypeid").(string)
	customerUserID := d.Get("customeruserid").(string)
	policyID, err := strconv.Atoi(d.Id())

	if err != nil {
		return nil,
			fmt.Errorf("error parsing Policy ID from state file: %v", err)
	}

	log.Printf("[INFO] Retrieving policy %d", policyID)
	client := api.NewRulesEngineAPIClient(*config)

	return client.GetPolicy(
		(**config).AccountNumber,
		customerUserID,
		portalTypeID,
		policyID)
}

func addPolicy(
	policy string,
	isEmptyPolicy bool,
	d *schema.ResourceData,
	m interface{},
) error {
	config := m.(**api.ClientConfig)
	customerID := d.Get("account_number").(string)
	customerUserID := d.Get("customeruserid").(string)

	// Portal Type IDs: 1=MCC 2=PCC 3=WCC 4=WCC 5=opencdn
	portalTypeID := d.Get("portaltypeid").(string)

	reClient := api.NewRulesEngineAPIClient(*config)
	parsedResponse, err :=
		reClient.AddPolicy(policy, customerID, portalTypeID, customerUserID)
	if err != nil {
		return fmt.Errorf("addPolicy: %v", err)
	}

	policyID, err := strconv.Atoi(parsedResponse.ID)
	if err != nil {
		return fmt.Errorf("addPolicy: parsing policy ID: %v", err)
	}

	if !isEmptyPolicy {
		d.SetId(parsedResponse.ID)
		d.Set("policy", policy)
	}

	deployRequest := getDeployRequestData(d, policyID)
	log.Printf(
		"[INFO] Deploying new policy for Account %s: %+v",
		customerID,
		deployRequest)

	deployResponse, deployErr := reClient.DeployPolicy(
		deployRequest,
		customerID,
		portalTypeID,
		customerUserID)

	if deployErr != nil {
		log.Printf(
			"[WARN] Deploying new policy for Account %s failed",
			customerID)
		return fmt.Errorf("addPolicy: %v", deployErr)
	}

	log.Printf(
		"[INFO] Successfully deployed new policy for Account %s: %+v",
		customerID,
		deployResponse)

	if isEmptyPolicy {
		d.SetId("") // indicates "delete" happened
	} else {
		d.Set("deploy_request_id", deployResponse.ID)
	}

	return nil
}

func cleanPolicyForTerrafomState(val interface{}) string {
	policy := val.(string)
	if len(policy) == 0 {
		return policy
	}
	policyMap := make(map[string]interface{})
	json.Unmarshal([]byte(policy), &policyMap)

	// remove unneeded metadata the user may have input
	cleanPolicy(policyMap)
	jsonBytes, err := json.Marshal(policyMap)
	if err != nil {
		panic(fmt.Errorf("cleanPolicyForTerrafomState: %v", err))
	}
	return string(jsonBytes)
}
