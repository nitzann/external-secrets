/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package externalsecret

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	esv1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	// Loading registered providers.
	_ "github.com/external-secrets/external-secrets/pkg/provider/register"
	"github.com/external-secrets/external-secrets/pkg/template"
	"github.com/external-secrets/external-secrets/pkg/utils"
)

type Parser struct {
	exec         template.ExecFunc
	dataMap      map[string][]byte
	client       client.Client
	targetSecret *v1.Secret
}

func (p *Parser) MergeConfigMap(ctx context.Context, namespace string, tpl esv1beta1.TemplateFrom) error {
	if tpl.ConfigMap == nil {
		return nil
	}
	var cm v1.ConfigMap
	err := p.client.Get(ctx, types.NamespacedName{
		Name:      tpl.ConfigMap.Name,
		Namespace: namespace,
	}, &cm)
	if err != nil {
		return err
	}
	for _, k := range tpl.ConfigMap.Items {
		val, ok := cm.Data[k.Key]
		out := make(map[string][]byte)
		if !ok {
			return fmt.Errorf(errTplCMMissingKey, tpl.ConfigMap.Name, k.Key)
		}
		switch k.TemplateAs {
		case esv1beta1.TemplateScopeValues:
			out[k.Key] = []byte(val)
		case esv1beta1.TemplateScopeKeysAndValues:
			out[val] = []byte(val)
		}
		err = p.exec(out, p.dataMap, k.TemplateAs, tpl.Target, p.targetSecret)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Parser) MergeSecret(ctx context.Context, namespace string, tpl esv1beta1.TemplateFrom) error {
	if tpl.Secret == nil {
		return nil
	}
	var sec v1.Secret
	err := p.client.Get(ctx, types.NamespacedName{
		Name:      tpl.Secret.Name,
		Namespace: namespace,
	}, &sec)
	if err != nil {
		return err
	}
	for _, k := range tpl.Secret.Items {
		val, ok := sec.Data[k.Key]
		if !ok {
			return fmt.Errorf(errTplSecMissingKey, tpl.Secret.Name, k.Key)
		}
		out := make(map[string][]byte)
		switch k.TemplateAs {
		case esv1beta1.TemplateScopeValues:
			out[k.Key] = val
		case esv1beta1.TemplateScopeKeysAndValues:
			out[string(val)] = val
		}
		err = p.exec(out, p.dataMap, k.TemplateAs, tpl.Target, p.targetSecret)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Parser) MergeLiteral(_ context.Context, tpl esv1beta1.TemplateFrom) error {
	if tpl.Literal == nil {
		return nil
	}
	out := make(map[string][]byte)
	out[*tpl.Literal] = []byte(*tpl.Literal)
	return p.exec(out, p.dataMap, esv1beta1.TemplateScopeKeysAndValues, tpl.Target, p.targetSecret)
}

func (p *Parser) MergeTemplateFrom(ctx context.Context, es *esv1beta1.ExternalSecret) error {
	if es.Spec.Target.Template == nil {
		return nil
	}
	for _, tpl := range es.Spec.Target.Template.TemplateFrom {
		err := p.MergeConfigMap(ctx, es.Namespace, tpl)
		if err != nil {
			return err
		}
		err = p.MergeSecret(ctx, es.Namespace, tpl)
		if err != nil {
			return err
		}
		err = p.MergeLiteral(ctx, tpl)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Parser) MergeMap(tplMap map[string]string, target esv1beta1.TemplateTarget) error {
	byteMap := make(map[string][]byte)
	for k, v := range tplMap {
		byteMap[k] = []byte(v)
	}
	err := p.exec(byteMap, p.dataMap, esv1beta1.TemplateScopeValues, target, p.targetSecret)
	if err != nil {
		return fmt.Errorf(errExecTpl, err)
	}
	return nil
}

// merge template in the following order:
// * template.Data (highest precedence)
// * template.templateFrom
// * secret via es.data or es.dataFrom.
func (r *Reconciler) applyTemplate(ctx context.Context, es *esv1beta1.ExternalSecret, secret *v1.Secret, dataMap map[string][]byte) error {
	if err := setMetadata(secret, es); err != nil {
		return err
	}

	// no template: copy data and return
	if es.Spec.Target.Template == nil {
		secret.Data = dataMap
		return nil
	}
	// Merge Policy should merge secrets
	if es.Spec.Target.Template.MergePolicy == esv1beta1.MergePolicyMerge {
		for k, v := range dataMap {
			secret.Data[k] = v
		}
	}
	execute, err := template.EngineForVersion(es.Spec.Target.Template.EngineVersion)
	if err != nil {
		return err
	}

	p := Parser{
		client:       r.Client,
		targetSecret: secret,
		dataMap:      dataMap,
		exec:         execute,
	}
	// apply templates defined in template.templateFrom
	err = p.MergeTemplateFrom(ctx, es)
	if err != nil {
		return fmt.Errorf(errFetchTplFrom, err)
	}
	// explicitly defined template.Data takes precedence over templateFrom
	err = p.MergeMap(es.Spec.Target.Template.Data, esv1beta1.TemplateTargetData)
	if err != nil {
		return fmt.Errorf(errExecTpl, err)
	}

	// get template data for labels
	err = p.MergeMap(es.Spec.Target.Template.Metadata.Labels, esv1beta1.TemplateTargetLabels)
	if err != nil {
		return fmt.Errorf(errExecTpl, err)
	}
	// get template data for annotations
	err = p.MergeMap(es.Spec.Target.Template.Metadata.Annotations, esv1beta1.TemplateTargetAnnotations)
	if err != nil {
		return fmt.Errorf(errExecTpl, err)
	}
	// if no data was provided by template fallback
	// to value from the provider
	if len(es.Spec.Target.Template.Data) == 0 && len(es.Spec.Target.Template.TemplateFrom) == 0 {
		secret.Data = dataMap
	}
	return nil
}

// setMetadata sets Labels and Annotations to the given secret.
func setMetadata(secret *v1.Secret, es *esv1beta1.ExternalSecret) error {
	if secret.Labels == nil {
		secret.Labels = make(map[string]string)
	}
	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	// Clean up Labels and Annotations added by the operator
	// so that it won't leave outdated ones
	labelKeys, err := getManagedLabelKeys(secret, es.Name)
	if err != nil {
		return err
	}
	for _, key := range labelKeys {
		delete(secret.ObjectMeta.Labels, key)
	}

	annotationKeys, err := getManagedAnnotationKeys(secret, es.Name)
	if err != nil {
		return err
	}
	for _, key := range annotationKeys {
		delete(secret.ObjectMeta.Annotations, key)
	}

	if es.Spec.Target.Template == nil {
		utils.MergeStringMap(secret.ObjectMeta.Labels, es.ObjectMeta.Labels)
		utils.MergeStringMap(secret.ObjectMeta.Annotations, es.ObjectMeta.Annotations)
		return nil
	}

	secret.Type = es.Spec.Target.Template.Type
	utils.MergeStringMap(secret.ObjectMeta.Labels, es.Spec.Target.Template.Metadata.Labels)
	utils.MergeStringMap(secret.ObjectMeta.Annotations, es.Spec.Target.Template.Metadata.Annotations)
	return nil
}

func getManagedAnnotationKeys(secret *v1.Secret, fieldOwner string) ([]string, error) {
	return getManagedFieldKeys(secret, fieldOwner, func(fields map[string]interface{}) []string {
		metadataFields, exists := fields["f:metadata"]
		if !exists {
			return nil
		}
		mf, ok := metadataFields.(map[string]interface{})
		if !ok {
			return nil
		}
		annotationFields, exists := mf["f:annotations"]
		if !exists {
			return nil
		}
		af, ok := annotationFields.(map[string]interface{})
		if !ok {
			return nil
		}
		var keys []string
		for k := range af {
			keys = append(keys, k)
		}
		return keys
	})
}

func getManagedLabelKeys(secret *v1.Secret, fieldOwner string) ([]string, error) {
	return getManagedFieldKeys(secret, fieldOwner, func(fields map[string]interface{}) []string {
		metadataFields, exists := fields["f:metadata"]
		if !exists {
			return nil
		}
		mf, ok := metadataFields.(map[string]interface{})
		if !ok {
			return nil
		}
		labelFields, exists := mf["f:labels"]
		if !exists {
			return nil
		}
		lf, ok := labelFields.(map[string]interface{})
		if !ok {
			return nil
		}
		var keys []string
		for k := range lf {
			keys = append(keys, k)
		}
		return keys
	})
}
