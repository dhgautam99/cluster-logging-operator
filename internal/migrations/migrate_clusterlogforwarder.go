package migrations

import (
	"fmt"
	log "github.com/ViaQ/logerr/v2/log/static"
	loggingv1 "github.com/openshift/cluster-logging-operator/apis/logging/v1"
	"github.com/openshift/cluster-logging-operator/internal/constants"
	"k8s.io/utils/strings/slices"
	"sort"
	"strings"
)

func MigrateClusterLogForwarderSpec(spec loggingv1.ClusterLogForwarderSpec, logStore *loggingv1.LogStoreSpec) loggingv1.ClusterLogForwarderSpec {
	spec = MigrateDefaultOutput(spec, logStore)
	return spec
}

// MigrateDefaultOutput adds the 'default' output spec to the list of outputs if it is not defined or
// selectively replaces it if it is.  It will apply OutputDefaults unless they are already defined.
func MigrateDefaultOutput(spec loggingv1.ClusterLogForwarderSpec, logStore *loggingv1.LogStoreSpec) loggingv1.ClusterLogForwarderSpec {
	// ClusterLogging without ClusterLogForwarder
	if len(spec.Pipelines) == 0 && len(spec.Inputs) == 0 && len(spec.Outputs) == 0 && spec.OutputDefaults == nil {
		if logStore != nil {
			log.V(2).Info("ClusterLogForwarder forwarding to default store")
			spec.Pipelines = []loggingv1.PipelineSpec{
				{
					InputRefs:  []string{loggingv1.InputNameApplication, loggingv1.InputNameInfrastructure},
					OutputRefs: []string{loggingv1.OutputNameDefault},
				},
			}
			if logStore.Type == loggingv1.LogStoreTypeElasticsearch {
				spec.Outputs = []loggingv1.OutputSpec{NewDefaultOutput(nil)}
			}
		}
	}

	if logStore != nil && logStore.Type == loggingv1.LogStoreTypeLokiStack {
		outputs, pipelines := processPipelinesForLokiStack(logStore, constants.OpenshiftNS, spec)
		spec.Outputs = append(spec.Outputs, outputs...)
		spec.Pipelines = pipelines
	}

	// Migrate ClusterLogForwarder
	routes := loggingv1.NewRoutes(spec.Pipelines)
	if _, ok := routes.ByOutput[loggingv1.OutputNameDefault]; ok {
		if logStore == nil {
			log.V(1).Info("ClusterLogForwarder references default logstore but one is not spec'd")
			return spec
		} else {
			replaced := false
			defaultOutput := NewDefaultOutput(spec.OutputDefaults)
			outputs := []loggingv1.OutputSpec{}
			for _, output := range spec.Outputs {
				if output.Name == loggingv1.OutputNameDefault {
					if output.Elasticsearch != nil {
						defaultOutput.Elasticsearch = output.Elasticsearch
					}
					output = defaultOutput
					replaced = true
				}
				outputs = append(outputs, output)
			}
			if !replaced {
				outputs = append(outputs, defaultOutput)
			}
			spec.Outputs = outputs
			return spec
		}
	}
	return spec
}

func NewDefaultOutput(defaults *loggingv1.OutputDefaults) loggingv1.OutputSpec {
	spec := loggingv1.OutputSpec{
		Name:   loggingv1.OutputNameDefault,
		Type:   loggingv1.OutputTypeElasticsearch,
		URL:    constants.LogStoreURL,
		Secret: &loggingv1.OutputSecretSpec{Name: constants.CollectorSecretName},
	}
	if defaults != nil && defaults.Elasticsearch != nil {
		spec.Elasticsearch = defaults.Elasticsearch
	}
	return spec
}

func processPipelinesForLokiStack(logStore *loggingv1.LogStoreSpec, namespace string, spec loggingv1.ClusterLogForwarderSpec) ([]loggingv1.OutputSpec, []loggingv1.PipelineSpec) {
	needOutput := make(map[string]bool)
	inPipelines := spec.Pipelines
	pipelines := []loggingv1.PipelineSpec{}

	for _, p := range inPipelines {
		if !slices.Contains(p.OutputRefs, loggingv1.OutputNameDefault) {
			// Skip pipelines that do not reference "default" output
			pipelines = append(pipelines, p)
			continue
		}

		for _, i := range p.InputRefs {
			needOutput[i] = true
		}

		for i, input := range p.InputRefs {
			pOut := p.DeepCopy()
			pOut.InputRefs = []string{input}

			for i, output := range pOut.OutputRefs {
				if output != loggingv1.OutputNameDefault {
					// Leave non-default output names as-is
					continue
				}

				pOut.OutputRefs[i] = lokiStackOutput(input)
			}

			if pOut.Name != "" && i > 0 {
				// Generate new name for named pipelines as duplicate names are not allowed
				pOut.Name = fmt.Sprintf("%s-%d", pOut.Name, i)
			}

			pipelines = append(pipelines, *pOut)
		}
	}

	outputs := []loggingv1.OutputSpec{}
	for input := range needOutput {
		tenant := getInputTypeFromName(spec, input)
		outputs = append(outputs, loggingv1.OutputSpec{
			Name: lokiStackOutput(input),
			Type: loggingv1.OutputTypeLoki,
			URL:  LokiStackURL(logStore, namespace, tenant),
		})
	}

	// Sort outputs, because we have tests depending on the exact generated configuration
	sort.Slice(outputs, func(i, j int) bool {
		return strings.Compare(outputs[i].Name, outputs[j].Name) < 0
	})

	return outputs, pipelines
}

func getInputTypeFromName(spec loggingv1.ClusterLogForwarderSpec, inputName string) string {
	if loggingv1.ReservedInputNames.Has(inputName) {
		// use name as type
		return inputName
	}

	for _, input := range spec.Inputs {
		if input.Name == inputName {
			if input.Application != nil {
				return loggingv1.InputNameApplication
			}
			if input.Infrastructure != nil {
				return loggingv1.InputNameInfrastructure
			}
			if input.Audit != nil {
				return loggingv1.InputNameAudit
			}
		}
	}
	log.V(3).Info("unable to get input type from name", "inputName", inputName)
	return ""
}

func lokiStackOutput(inputName string) string {
	switch inputName {
	case loggingv1.InputNameApplication:
		return loggingv1.OutputNameDefault + "-loki-apps"
	case loggingv1.InputNameInfrastructure:
		return loggingv1.OutputNameDefault + "-loki-infra"
	case loggingv1.InputNameAudit:
		return loggingv1.OutputNameDefault + "-loki-audit"
	}

	return loggingv1.OutputNameDefault + "-" + inputName
}

// LokiStackGatewayService returns the name of LokiStack gateway service.
// Returns an empty string if ClusterLogging is not configured for a LokiStack log store.
func LokiStackGatewayService(logStore *loggingv1.LogStoreSpec) string {
	if logStore == nil || logStore.LokiStack.Name == "" {
		return ""
	}

	return fmt.Sprintf("%s-gateway-http", logStore.LokiStack.Name)
}

// LokiStackURL returns the URL of the LokiStack API for a specific tenant.
// Returns an empty string if ClusterLogging is not configured for a LokiStack log store.
func LokiStackURL(logStore *loggingv1.LogStoreSpec, namespace, tenant string) string {
	service := LokiStackGatewayService(logStore)
	if service == "" {
		return ""
	}
	if !loggingv1.ReservedInputNames.Has(tenant) {
		log.V(3).Info("url tenant must be one of our reserved input names", "tenant", tenant)
		return ""
	}

	return fmt.Sprintf("https://%s.%s.svc:8080/api/logs/v1/%s", service, namespace, tenant)
}
