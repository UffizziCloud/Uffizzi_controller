package domain

import (
	"fmt"
	"log"

	"gitlab.com/dualbootpartners/idyl/uffizzi_controller/internal/global"
	domainTypes "gitlab.com/dualbootpartners/idyl/uffizzi_controller/internal/types/domain"
	corev1 "k8s.io/api/core/v1"
)

func (l *Logic) ResetNamespaceErrors(namespace *corev1.Namespace) (*corev1.Namespace, error) {
	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}

	namespace.Annotations["errors"] = ""

	namespace, err := l.KuberClient.UpdateNamespace(namespace)
	if err != nil {
		return nil, err
	}

	return namespace, err
}

func (l *Logic) handleDomainDeploymentError(namespaceName string, domainErr error) error {
	log.Printf("DomainError: %s", domainErr)

	err := l.MarkUnresponsiveContainersAsFailed(namespaceName)
	if err != nil {
		return err
	}

	_, err = l.KuberClient.UpdateAnnotationNamespace(namespaceName, "errors", domainErr.Error())
	if err != nil {
		return err
	}

	return nil
}

func (l *Logic) CleaningNamespaceForEmptyContainers(namespace *corev1.Namespace, deploymentName string) error {
	serviceName := namespace.Annotations["serviceName"]
	ingressName := namespace.Annotations["ingressName"]

	namespace.Annotations["serviceName"] = ""
	namespace.Annotations["ingressName"] = ""

	_, err := l.KuberClient.UpdateNamespace(namespace)
	if err != nil {
		return err
	}

	err = l.KuberClient.RemoveDeployments(namespace.Name, deploymentName)
	if err != nil {
		return err
	}

	log.Printf("deployments were removed")

	if len(serviceName) > 0 {
		err = l.KuberClient.RemoveService(namespace.Name, serviceName)
		if err != nil {
			return err
		}

		log.Printf("services were removed")
	}

	if len(ingressName) > 0 {
		err = l.KuberClient.RemoveIngress(namespace.Name, ingressName)
		if err != nil {
			return err
		}

		log.Printf("ingress were removed")
	}

	return nil
}

func (l *Logic) ApplyContainers(
	deploymentID uint64,
	containerList domainTypes.ContainerList,
	credentials []domainTypes.Credential,
	deploymentHost string,
) error {
	namespaceName := l.KubernetesNamespaceName(deploymentID)

	namespace, err := l.KuberClient.FindNamespace(namespaceName)
	if err != nil {
		return err
	}

	log.Printf("namespace/%s found", namespace.Name)

	namespace, err = l.ResetNamespaceErrors(namespace)
	if err != nil {
		return err
	}

	appName := fmt.Sprintf("app-%s", namespace.Name)
	deploymentName := appName
	deploymentSelectorName := appName

	policy, err := l.KuberClient.FindOrCreateNetworkPolicy(namespace.Name, appName)
	if err != nil {
		return err
	}

	log.Printf("networkPolicy/%s configured", policy.Name)
	log.Printf("namespace/%s containerList: %+#v", namespace.Name, containerList)

	err = l.ClearOldConfigurationFiles(namespace, containerList)
	if err != nil {
		return err
	}

	err = l.ApplyContainerSecrets(namespaceName, containerList)
	if err != nil {
		return err
	}

	if containerList.IsEmpty() {
		return l.CleaningNamespaceForEmptyContainers(namespace, appName)
	}

	err = l.ResetNetworkConnectivityTemplate(namespace, containerList)
	if err != nil {
		return l.handleDomainDeploymentError(namespace.Name, err)
	}

	deployment, err := l.KuberClient.CreateOrUpdateDeployments(
		namespace,
		deploymentName,
		deploymentSelectorName,
		containerList,
		credentials,
	)
	if err != nil {
		return l.handleDomainDeploymentError(namespace.Name, err)
	}

	log.Printf("deployment/%s configured", deployment.Name)

	shouldAutoscale := namespace.Labels["kind"] == domainTypes.DeploymentTypeEnterprise ||
		namespace.Labels["kind"] == domainTypes.DeploymentTypePerformance

	minReplicas := global.Settings.AutoscalingMinPerformanceReplicas
	maxReplicas := global.Settings.AutoscalingMaxPerformanceReplicas

	if namespace.Labels["kind"] == domainTypes.DeploymentTypeEnterprise {
		minReplicas = global.Settings.AutoscalingMinEnterpriseReplicas
		maxReplicas = global.Settings.AutoscalingMaxEnterpriseReplicas
	}

	if !shouldAutoscale {
		err := l.KuberClient.DeleteHorizontalPodAutoscalerIfExists(
			namespace,
			deploymentName,
		)
		if err != nil {
			return l.handleDomainDeploymentError(namespace.Name, err)
		}

		log.Printf("Removed Horizontal Pod Autoscaler (if one existed) from %s.", deploymentName)
	} else {
		autoscaler, err := l.KuberClient.CreateOrUpdateHorizontalPodAutoscaler(
			namespace,
			deploymentName,
			minReplicas,
			maxReplicas,
		)
		if err != nil {
			return l.handleDomainDeploymentError(namespace.Name, err)
		}

		log.Printf("Horizontal Pod Autoscaler %s created.", autoscaler.Name)
	}

	var networkBuilder INetworkBuilder

	networkDependencies := NewNetworkDependencies(l, namespace, appName, containerList, deployment, deploymentHost)

	networkBuilder = NewIngressNetworkBuilder(networkDependencies)

	err = networkBuilder.Create()
	if err != nil {
		return l.handleDomainDeploymentError(namespace.Name, err)
	}

	err = networkBuilder.AwaitNetworkCreation()
	if err != nil {
		return l.handleDomainDeploymentError(namespace.Name, err)
	}

	log.Printf("UffizziDeployment/%d configured", deploymentID)

	return nil
}
