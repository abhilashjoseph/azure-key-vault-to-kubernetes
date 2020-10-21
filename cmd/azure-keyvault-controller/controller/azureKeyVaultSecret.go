/*
Copyright Sparebanken Vest

Based on the Kubernetes controller example at
https://github.com/kubernetes/sample-controller

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

package controller

import (
	"fmt"

	"github.com/SparebankenVest/azure-key-vault-to-kubernetes/pkg/akv2k8s/transformers"
	akv "github.com/SparebankenVest/azure-key-vault-to-kubernetes/pkg/k8s/apis/azurekeyvault/v2alpha1"
	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"

	"kmodules.xyz/client-go/tools/queue"
)

func (c *Controller) initAzureKeyVaultSecret() {
	c.akvsInformerFactory.Keyvault().V2alpha1().AzureKeyVaultSecrets().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			secret, err := convertToAzureKeyVaultSecret(obj)
			if err != nil {
				log.Errorf("failed to convert to azurekeyvaultsecret: %v", err)
			}

			if c.akvsHasOutputDefined(secret) {
				log.Debugf("AzureKeyVaultSecret %s/%s added. Adding to queue.", secret.Namespace, secret.Name)
				queue.Enqueue(c.akvsCrdQueue.GetQueue(), obj)
				// queue.Enqueue(c.azureKeyVaultQueue.GetQueue(), obj)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			newSecret, err := convertToAzureKeyVaultSecret(new)
			if err != nil {
				log.Errorf("failed to convert to azurekeyvaultsecret: %v", err)
			}

			oldSecret, err := convertToAzureKeyVaultSecret(old)
			if err != nil {
				log.Errorf("failed to convert to azurekeyvaultsecret: %v", err)
			}

			// If akvs has not changed and has secret output, add to akv queue to check if secret has changed in akv
			if newSecret.ResourceVersion == oldSecret.ResourceVersion && c.akvsHasOutputDefined(newSecret) {
				log.Debugf("AzureKeyVaultSecret %s/%s not changed. Adding to Azure Key Vault queue to check if secret has changed in Azure Key Vault.", newSecret.Namespace, newSecret.Name)
				queue.Enqueue(c.azureKeyVaultQueue.GetQueue(), new)
				return
			}

			if c.akvsHasOutputDefined(newSecret) || c.akvsHasOutputDefined(oldSecret) {
				log.Debugf("AzureKeyVaultSecret %s/%s changed. Adding to queue.", newSecret.Namespace, newSecret.Name)
				queue.Enqueue(c.akvsCrdQueue.GetQueue(), new)
			}
		},
		DeleteFunc: func(obj interface{}) {
			secret, err := convertToAzureKeyVaultSecret(obj)
			if err != nil {
				log.Errorf("failed to convert to azurekeyvaultsecret: %v", err)
			}

			if c.akvsHasOutputDefined(secret) {
				log.Debugf("AzureKeyVaultSecret %s/%s deleted. Adding to delete queue.", secret.Namespace, secret.Name)
				queue.Enqueue(c.akvsCrdQueue.GetQueue(), obj)

				// Getting default key to remove from Azure work queue
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err != nil {
					utilruntime.HandleError(err)
					return
				}
				c.azureKeyVaultQueue.GetQueue().Forget(key)
			}
		},
	})
}

func (c *Controller) syncAzureKeyVaultSecret(key string) error {
	var akvs *akv.AzureKeyVaultSecret
	var err error

	log.Debugf("Processing AzureKeyVaultSecret %s", key)
	if akvs, err = c.getAzureKeyVaultSecret(key); err != nil {
		if exit := handleKeyVaultError(err, key); exit {
			return nil
		}
		return err
	}

	var outputObject metav1.Object
	if c.akvsHasOutputSecret(akvs) {
		secret, err := c.getOrCreateKubernetesSecret(akvs)
		if err != nil {
			return err
		}

		log.Debugf("successfully synced azurekeyvaultsecret %s with kubernetes secret %s", key, fmt.Sprintf("%s/%s", secret.Namespace, secret.Name))
		c.recorder.Event(secret, corev1.EventTypeNormal, SuccessSynced, MessageAzureKeyVaultSecretSynced)
		outputObject = secret
	}

	if c.akvsHasOutputConfigMap(akvs) {
		cm, err := c.getOrCreateKubernetesConfigMap(akvs)
		if err != nil {
			return err
		}

		log.Debugf("successfully synced azurekeyvaultsecret %s with kubernetes configmap %s", key, fmt.Sprintf("%s/%s", cm.Namespace, cm.Name))
		c.recorder.Event(cm, corev1.EventTypeNormal, SuccessSynced, MessageAzureKeyVaultSecretSynced)
		outputObject = cm
	}

	if !isOwnedBy(outputObject, akvs) { // checks if the object has a controllerRef set to the given owner
		msg := fmt.Sprintf(MessageResourceExists, outputObject.GetName())
		log.Warning(msg)
		c.recorder.Event(akvs, corev1.EventTypeWarning, ErrResourceExists, msg)
		return fmt.Errorf(msg)
	}

	return nil
}

func (c *Controller) syncAzureKeyVault(key string) error {
	var akvs *akv.AzureKeyVaultSecret
	var err error

	log.Debugf("Checking state for %s in Azure", key)
	if akvs, err = c.getAzureKeyVaultSecret(key); err != nil {
		if exit := handleKeyVaultError(err, key); exit {
			return nil
		}
		return err
	}

	if c.akvsHasOutputSecret(akvs) {
		log.Debugf("Getting secret value for %s in Azure", key)
		secretValue, err := c.getSecretFromKeyVault(akvs)
		if err != nil {
			msg := fmt.Sprintf(FailedAzureKeyVault, akvs.Name, akvs.Spec.Vault.Name)
			log.Errorf("failed to get secret value for '%s' from Azure Key vault '%s' using object name '%s', error: %+v", key, akvs.Spec.Vault.Name, akvs.Spec.Vault.Object.Name, err)
			c.recorder.Event(akvs, corev1.EventTypeWarning, ErrAzureVault, msg)
			return fmt.Errorf(msg)
		}

		akvsValuesHash := getMD5HashOfByteValues(secretValue)

		log.Debugf("Checking if secret value for %s has changed in Azure", key)
		if akvs.Status.SecretHash != akvsValuesHash {
			log.Infof("Secret has changed in Azure Key Vault for AzureKeyvVaultSecret %s. Updating Secret now.", akvs.Name)

			existingSecret, err := c.kubeclientset.CoreV1().Secrets(akvs.Namespace).Get(akvs.Spec.Output.Secret.Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get existing secret %s, error: %+v", akvs.Spec.Output.Secret.Name, err)
			}

			updatedSecret, err := updateExistingSecret(akvs, secretValue, existingSecret)
			if err != nil {
				return fmt.Errorf("failed to update existing secret %s, error: %+v", akvs.Spec.Output.Secret.Name, err)
			}

			secret, err := c.kubeclientset.CoreV1().Secrets(akvs.Namespace).Update(updatedSecret)
			if err != nil {
				return fmt.Errorf("failed to update secret, error: %+v", err)
			}

			log.Warningf("Secret value will now change for Secret '%s'. Any resources (like Pods) using this Secret must be restarted to pick up the new value. Details: https://github.com/kubernetes/kubernetes/issues/22368", secret.Name)
		}

		log.Debugf("Updating status for AzureKeyVaultSecret '%s'", akvs.Name)
		if err = c.updateAzureKeyVaultSecretStatusForSecret(akvs, akvsValuesHash); err != nil {
			return err
		}
	}

	if c.akvsHasOutputConfigMap(akvs) {
		log.Debugf("Getting secret value for %s in Azure", key)
		cmValue, err := c.getConfigMapFromKeyVault(akvs)
		if err != nil {
			msg := fmt.Sprintf(FailedAzureKeyVault, akvs.Name, akvs.Spec.Vault.Name)
			log.Errorf("failed to get secret value for '%s' from Azure Key vault '%s' using object name '%s', error: %+v", key, akvs.Spec.Vault.Name, akvs.Spec.Vault.Object.Name, err)
			c.recorder.Event(akvs, corev1.EventTypeWarning, ErrAzureVault, msg)
			return fmt.Errorf(msg)
		}

		cmHash := getMD5HashOfStringValues(cmValue)

		log.Debugf("Checking if secret value for %s has changed in Azure", key)
		if akvs.Status.ConfigMapHash != cmHash {
			log.Infof("Secret has changed in Azure Key Vault for AzureKeyvVaultSecret %s. Updating Secret now.", akvs.Name)

			cm, err := c.kubeclientset.CoreV1().ConfigMaps(akvs.Namespace).Update(createNewConfigMap(akvs, cmValue))
			if err != nil {
				log.Warningf("Failed to create Secret, Error: %+v", err)
				return err
			}

			log.Warningf("Secret value will now change for Secret '%s'. Any resources (like Pods) using this Secret must be restarted to pick up the new value. Details: https://github.com/kubernetes/kubernetes/issues/22368", cm.Name)
		}

		log.Debugf("Updating status for AzureKeyVaultSecret '%s'", akvs.Name)
		if err = c.updateAzureKeyVaultSecretStatusForConfigMap(akvs, cmHash); err != nil {
			return err
		}
	}

	log.Debugf("Successfully synced AzureKeyVaultSecret %s with Azure Key Vault", key)
	c.recorder.Event(akvs, corev1.EventTypeNormal, SuccessSynced, MessageAzureKeyVaultSecretSyncedWithAzureKeyVault)
	return nil
}

func convertToAzureKeyVaultSecret(obj interface{}) (*akv.AzureKeyVaultSecret, error) {
	secret, ok := obj.(*akv.AzureKeyVaultSecret)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return nil, fmt.Errorf("couldn't get object from tombstone %#v", obj)
		}
		secret, ok = tombstone.Obj.(*akv.AzureKeyVaultSecret)
		if !ok {
			return nil, fmt.Errorf("tombstone contained object that is not a AzureKeyVaultSecret %#v", obj)
		}
	}
	return secret, nil
}

func (c *Controller) akvsHasOutputDefined(secret *akv.AzureKeyVaultSecret) bool {
	return c.akvsHasOutputSecret(secret) || c.akvsHasOutputConfigMap(secret)
}

func (c *Controller) akvsHasOutputSecret(secret *akv.AzureKeyVaultSecret) bool {
	return secret.Spec.Output.Secret.Name != ""
}

func (c *Controller) akvsHasOutputConfigMap(secret *akv.AzureKeyVaultSecret) bool {
	return secret.Spec.Output.ConfigMap.Name != ""
}

func (c *Controller) getAzureKeyVaultSecretFromSecret(secret *corev1.Secret, owner *metav1.OwnerReference) (*akv.AzureKeyVaultSecret, error) {
	return c.azureKeyVaultSecretLister.AzureKeyVaultSecrets(secret.Namespace).Get(owner.Name)
}

func (c *Controller) getAzureKeyVaultSecretFromConfigMap(cm *corev1.ConfigMap, owner *metav1.OwnerReference) (*akv.AzureKeyVaultSecret, error) {
	return c.azureKeyVaultSecretLister.AzureKeyVaultSecrets(cm.Namespace).Get(owner.Name)
}

func (c *Controller) isOwnedByAzureKeyVaultSecret(obj metav1.Object) bool {
	if ownerRef := metav1.GetControllerOf(obj); ownerRef != nil {
		if ownerRef.Kind == "AzureKeyVaultSecret" {
			return true
		}
	}
	return false
}

func (c *Controller) getSecretFromKeyVault(azureKeyVaultSecret *akv.AzureKeyVaultSecret) (map[string][]byte, error) {
	var secretHandler KubernetesHandler

	switch azureKeyVaultSecret.Spec.Vault.Object.Type {
	case akv.AzureKeyVaultObjectTypeSecret:
		transformator, err := transformers.CreateTransformator(&azureKeyVaultSecret.Spec.Output)
		if err != nil {
			return nil, err
		}
		secretHandler = NewAzureSecretHandler(azureKeyVaultSecret, c.vaultService, *transformator)
	case akv.AzureKeyVaultObjectTypeCertificate:
		secretHandler = NewAzureCertificateHandler(azureKeyVaultSecret, c.vaultService)
	case akv.AzureKeyVaultObjectTypeKey:
		secretHandler = NewAzureKeyHandler(azureKeyVaultSecret, c.vaultService)
	case akv.AzureKeyVaultObjectTypeMultiKeyValueSecret:
		secretHandler = NewAzureMultiKeySecretHandler(azureKeyVaultSecret, c.vaultService)
	default:
		return nil, fmt.Errorf("azure key vault object type '%s' not currently supported", azureKeyVaultSecret.Spec.Vault.Object.Type)
	}
	return secretHandler.HandleSecret()
}

func (c *Controller) getConfigMapFromKeyVault(azureKeyVaultSecret *akv.AzureKeyVaultSecret) (map[string]string, error) {
	var cmHandler KubernetesHandler

	switch azureKeyVaultSecret.Spec.Vault.Object.Type {
	case akv.AzureKeyVaultObjectTypeSecret:
		transformator, err := transformers.CreateTransformator(&azureKeyVaultSecret.Spec.Output)
		if err != nil {
			return nil, err
		}
		cmHandler = NewAzureSecretHandler(azureKeyVaultSecret, c.vaultService, *transformator)
	case akv.AzureKeyVaultObjectTypeCertificate:
		cmHandler = NewAzureCertificateHandler(azureKeyVaultSecret, c.vaultService)
	case akv.AzureKeyVaultObjectTypeKey:
		cmHandler = NewAzureKeyHandler(azureKeyVaultSecret, c.vaultService)
	case akv.AzureKeyVaultObjectTypeMultiKeyValueSecret:
		cmHandler = NewAzureMultiKeySecretHandler(azureKeyVaultSecret, c.vaultService)
	default:
		return nil, fmt.Errorf("azure key vault object type '%s' not currently supported", azureKeyVaultSecret.Spec.Vault.Object.Type)
	}
	return cmHandler.HandleConfigMap()
}

func (c *Controller) getAzureKeyVaultSecret(key string) (*akv.AzureKeyVaultSecret, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("invalid resource key: %s", key)
	}

	log.Debugf("Getting AzureKeyVaultSecret %s from namespace %s", name, namespace)
	azureKeyVaultSecret, err := c.azureKeyVaultSecretLister.AzureKeyVaultSecrets(namespace).Get(name)

	if err != nil {
		return nil, err
	}
	return azureKeyVaultSecret, err
}

func hasAzureKeyVaultSecretChangedForSecret(akvs *akv.AzureKeyVaultSecret, akvsValues map[string][]byte, secret *corev1.Secret) bool {
	// check if secret type has changed
	secretType := determineSecretType(akvs)
	if secretType != secret.Type {
		return true
	}

	// Check if dataKey has changed by trying to lookup key
	if akvs.Spec.Output.Secret.DataKey != "" {
		if _, ok := secret.Data[akvs.Spec.Output.Secret.DataKey]; !ok {
			return true
		}
	}

	// Check if data content has changed
	if akvs.Status.SecretHash != getMD5HashOfSecret(akvsValues, secret) {
		return true
	}
	return false
}

func hasAzureKeyVaultSecretChangedForConfigMap(akvs *akv.AzureKeyVaultSecret, akvsValues map[string]string, cm *corev1.ConfigMap) bool {
	// Check if dataKey has changed by trying to lookup key
	if akvs.Spec.Output.ConfigMap.DataKey != "" {
		if _, ok := cm.Data[akvs.Spec.Output.ConfigMap.DataKey]; !ok {
			return true
		}
	}

	// Check if data content has changed
	if akvs.Status.ConfigMapHash != getMD5HashOfConfigMap(akvsValues, cm) {
		return true
	}
	return false
}

func (c *Controller) updateAzureKeyVaultSecretStatus(akvs *akv.AzureKeyVaultSecret, secretName, cmName, secretHash, cmHash string) error {
	akvsCopy := akvs.DeepCopy()
	akvsCopy.Status.SecretName = secretName
	akvsCopy.Status.SecretHash = secretHash
	akvsCopy.Status.ConfigMapName = cmName
	akvsCopy.Status.ConfigMapHash = cmHash
	akvsCopy.Status.LastAzureUpdate = c.clock.Now()

	_, err := c.akvsClient.KeyvaultV2alpha1().AzureKeyVaultSecrets(akvs.Namespace).UpdateStatus(akvsCopy)
	return err
}

func (c *Controller) updateAzureKeyVaultSecretStatusForSecret(akvs *akv.AzureKeyVaultSecret, secretHash string) error {
	secretName := determineSecretName(akvs)

	akvsCopy := akvs.DeepCopy()
	akvsCopy.Status.SecretName = secretName
	akvsCopy.Status.SecretHash = secretHash
	akvsCopy.Status.LastAzureUpdate = c.clock.Now()

	_, err := c.akvsClient.KeyvaultV2alpha1().AzureKeyVaultSecrets(akvs.Namespace).UpdateStatus(akvsCopy)
	return err
}

func (c *Controller) updateAzureKeyVaultSecretStatusForConfigMap(akvs *akv.AzureKeyVaultSecret, cmHash string) error {
	cmName := determineConfigMapName(akvs)

	akvsCopy := akvs.DeepCopy()
	akvsCopy.Status.ConfigMapName = cmName
	akvsCopy.Status.ConfigMapHash = cmHash
	akvsCopy.Status.LastAzureUpdate = c.clock.Now()

	_, err := c.akvsClient.KeyvaultV2alpha1().AzureKeyVaultSecrets(akvs.Namespace).UpdateStatus(akvsCopy)
	return err
}

func handleKeyVaultError(err error, key string) bool {
	log.Debugf("Handling error for '%s' in AzureKeyVaultSecret: %s", key, err.Error())
	exit := false
	if err != nil {
		// The AzureKeyVaultSecret resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			log.Debugf("Error for '%s' was 'Not Found'", key)

			log.Errorf("AzureKeyVaultSecret '%s' in work queue no longer exists", key)
			exit = true
		}
	}
	return exit
}
