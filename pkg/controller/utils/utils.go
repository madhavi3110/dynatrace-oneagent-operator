package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	b64 "encoding/base64"

	dynatracev1alpha1 "github.com/Dynatrace/dynatrace-oneagent-operator/pkg/apis/dynatrace/v1alpha1"
	"github.com/Dynatrace/dynatrace-oneagent-operator/pkg/dtclient"
	"github.com/go-logr/logr"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	DynatracePaasToken = "paasToken"
	DynatraceApiToken  = "apiToken"
)

var logger = log.Log.WithName("dynatrace.utils")

// DynatraceClientFunc defines handler func for dynatrace client
type DynatraceClientFunc func(rtc client.Client, instance dynatracev1alpha1.BaseOneAgent, hasAPIToken, hasPaaSToken bool) (dtclient.Client, error)

// BuildDynatraceClient creates a new Dynatrace client using the settings configured on the given instance.
func BuildDynatraceClient(rtc client.Client, instance dynatracev1alpha1.BaseOneAgent, hasAPIToken, hasPaaSToken bool) (dtclient.Client, error) {
	ns := instance.GetNamespace()
	spec := instance.GetSpec()

	secret := &corev1.Secret{}
	err := rtc.Get(context.TODO(), client.ObjectKey{Name: GetTokensName(instance), Namespace: ns}, secret)
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, err
	}

	// initialize dynatrace client
	var opts []dtclient.Option
	if spec.SkipCertCheck {
		opts = append(opts, dtclient.SkipCertificateValidation(true))
	}

	if p := spec.Proxy; p != nil {
		if p.ValueFrom != "" {
			proxySecret := &corev1.Secret{}
			err := rtc.Get(context.TODO(), client.ObjectKey{Name: p.ValueFrom, Namespace: ns}, proxySecret)
			if err != nil {
				return nil, fmt.Errorf("failed to get proxy secret: %w", err)
			}

			proxyURL, err := extractToken(proxySecret, "proxy")
			if err != nil {
				return nil, fmt.Errorf("failed to extract proxy secret field: %w", err)
			}
			opts = append(opts, dtclient.Proxy(proxyURL))
		} else if p.Value != "" {
			opts = append(opts, dtclient.Proxy(p.Value))
		}
	}

	if spec.TrustedCAs != "" {
		certs := &corev1.ConfigMap{}
		if err := rtc.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: spec.TrustedCAs}, certs); err != nil {
			return nil, fmt.Errorf("failed to get certificate configmap: %w", err)
		}
		if certs.Data["certs"] == "" {
			return nil, fmt.Errorf("failed to extract certificate configmap field: missing field certs")
		}
		opts = append(opts, dtclient.Certs([]byte(certs.Data["certs"])))
	}

	if spec.NetworkZone != "" {
		opts = append(opts, dtclient.NetworkZone(spec.NetworkZone))
	}

	var apiToken string
	if hasAPIToken {
		if apiToken, err = extractToken(secret, DynatraceApiToken); err != nil {
			return nil, err
		}
	}

	var paasToken string
	if hasPaaSToken {
		if paasToken, err = extractToken(secret, DynatracePaasToken); err != nil {
			return nil, err
		}
	}

	return dtclient.NewClient(spec.APIURL, apiToken, paasToken, opts...)
}

func extractToken(secret *v1.Secret, key string) (string, error) {
	value, ok := secret.Data[key]
	if !ok {
		err := fmt.Errorf("missing token %s", key)
		return "", err
	}

	return strings.TrimSpace(string(value)), nil
}

// StaticDynatraceClient creates a DynatraceClientFunc always returning c.
func StaticDynatraceClient(c dtclient.Client) DynatraceClientFunc {
	return func(_ client.Client, oa dynatracev1alpha1.BaseOneAgent, _, _ bool) (dtclient.Client, error) {
		return c, nil
	}
}

func GetTokensName(obj dynatracev1alpha1.BaseOneAgent) string {
	if tkns := obj.GetSpec().Tokens; tkns != "" {
		return tkns
	}
	return obj.GetName()
}

// GetDeployment returns the Deployment object who is the owner of this pod.
func GetDeployment(c client.Client, ns string) (*appsv1.Deployment, error) {
	pod, err := k8sutil.GetPod(context.TODO(), c, ns)
	if err != nil {
		return nil, err
	}

	rsOwner := metav1.GetControllerOf(pod)
	if rsOwner == nil {
		return nil, fmt.Errorf("no controller found for Pod: %s", pod.Name)
	} else if rsOwner.Kind != "ReplicaSet" {
		return nil, fmt.Errorf("unexpected controller found for Pod: %s, kind: %s", pod.Name, rsOwner.Kind)
	}

	var rs appsv1.ReplicaSet
	if err := c.Get(context.TODO(), client.ObjectKey{Name: rsOwner.Name, Namespace: ns}, &rs); err != nil {
		return nil, err
	}

	dOwner := metav1.GetControllerOf(&rs)
	if dOwner == nil {
		return nil, fmt.Errorf("no controller found for ReplicaSet: %s", pod.Name)
	} else if dOwner.Kind != "Deployment" {
		return nil, fmt.Errorf("unexpected controller found for ReplicaSet: %s, kind: %s", pod.Name, dOwner.Kind)
	}

	var d appsv1.Deployment
	if err := c.Get(context.TODO(), client.ObjectKey{Name: dOwner.Name, Namespace: ns}, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateOrUpdateSecretIfNotExists creates a secret in case it does not exist or updates it if there are changes
func CreateOrUpdateSecretIfNotExists(c client.Client, r client.Reader, secretName string, targetNS string, data map[string][]byte, secretType corev1.SecretType, log logr.Logger) error {
	var cfg corev1.Secret
	err := r.Get(context.TODO(), client.ObjectKey{Name: secretName, Namespace: targetNS}, &cfg)
	if errors.IsNotFound(err) {
		log.Info("Creating OneAgent config secret")
		if err := c.Create(context.TODO(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: targetNS,
			},
			Type: secretType,
			Data: data,
		}); err != nil {
			return fmt.Errorf("failed to create secret %s: %w", secretName, err)
		}
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to query for secret %s: %w", secretName, err)
	}

	if !reflect.DeepEqual(data, cfg.Data) {
		log.Info(fmt.Sprintf("Updating secret %s", secretName))
		cfg.Data = data
		if err := c.Update(context.TODO(), &cfg); err != nil {
			return fmt.Errorf("failed to update secret %s: %w", secretName, err)
		}
	}

	return nil
}

// GeneratePullSecretData generates the secret data for the PullSecret
func GeneratePullSecretData(c client.Client, apm *dynatracev1alpha1.OneAgentAPM, tkns *corev1.Secret) (map[string][]byte, error) {
	type auths struct {
		Username string
		Password string
		Auth     string
	}

	type dockercfg struct {
		Auths map[string]auths
	}

	dtc, err := BuildDynatraceClient(c, apm, false, true)
	if err != nil {
		return nil, err
	}

	ci, err := dtc.GetConnectionInfo()
	if err != nil {
		return nil, err
	}

	r, err := GetImageRegistryFromAPIURL(apm.Spec.APIURL)
	if err != nil {
		return nil, err
	}

	a := fmt.Sprintf("%s:%s", ci.TenantUUID, string(tkns.Data[DynatracePaasToken]))
	a = b64.StdEncoding.EncodeToString([]byte(a))

	auth := auths{
		Username: ci.TenantUUID,
		Password: string(tkns.Data[DynatracePaasToken]),
		Auth:     a,
	}

	d := dockercfg{
		Auths: map[string]auths{
			r: auth,
		},
	}
	j, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}

	return map[string][]byte{".dockerconfigjson": j}, nil
}

// BuildOneAgentAPMImage builds the docker image for the agentapm based on the api url
// If annotations are set (flavor or technologies) they get appended
func BuildOneAgentAPMImage(apiURL string, flavor string, technologies string, agentVersion string) (string, error) {
	var tags []string

	registry, err := GetImageRegistryFromAPIURL(apiURL)
	if err != nil {
		return "", err
	}

	image := registry + "/linux/codemodule"

	if flavor != "default" {
		image += "-musl"
	}

	if technologies != "all" {
		tags = append(tags, strings.Split(technologies, ",")...)
	}

	if agentVersion != "" {
		tags = append(tags, agentVersion)
	}

	if len(tags) > 0 {
		image = fmt.Sprintf("%s:%s", image, strings.Join(tags, "-"))
	}

	return image, nil
}

func GetImageRegistryFromAPIURL(apiURL string) (string, error) {
	r := strings.TrimPrefix(apiURL, "https://")
	r = strings.TrimSuffix(r, "/api")
	return r, nil
}

func GetField(values map[string]string, key, defaultValue string) string {
	if values == nil {
		return defaultValue
	}
	if x := values[key]; x != "" {
		return x
	}
	return defaultValue
}
