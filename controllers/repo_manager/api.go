/*
Copyright 2022.

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

package repo_manager

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	repomanagerpulpprojectorgv1beta2 "github.com/pulp/pulp-operator/apis/repo-manager.pulpproject.org/v1beta2"
	"github.com/pulp/pulp-operator/controllers"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ApiResource has the definition and function to provision api objects
type ApiResource struct {
	Definition ResourceDefinition
	Function   func(FunctionResources) client.Object
}

// pulpApiController provision and reconciles api objects
func (r *RepoManagerReconciler) pulpApiController(ctx context.Context, pulp *repomanagerpulpprojectorgv1beta2.Pulp, log logr.Logger) (ctrl.Result, error) {

	// conditionType is used to update .status.conditions with the current resource state
	conditionType := cases.Title(language.English, cases.Compact).String(pulp.Spec.DeploymentType) + "-API-Ready"
	funcResources := FunctionResources{ctx, pulp, log, r}

	// pulp-file-storage
	// the PVC will be created only if a StorageClassName is provided
	if storageClassProvided(pulp) {
		requeue, err := r.createPulpResource(ResourceDefinition{ctx, &corev1.PersistentVolumeClaim{}, pulp.Name + "-file-storage", "FileStorage", conditionType, pulp}, fileStoragePVC)
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			return ctrl.Result{Requeue: true}, nil
		}

		// Reconcile PVC
		pvcFound := &corev1.PersistentVolumeClaim{}
		r.Get(ctx, types.NamespacedName{Name: pulp.Name + "-file-storage", Namespace: pulp.Namespace}, pvcFound)
		expected_pvc := fileStoragePVC(funcResources)
		if !equality.Semantic.DeepDerivative(expected_pvc.(*corev1.PersistentVolumeClaim).Spec, pvcFound.Spec) {
			log.Info("The PVC has been modified! Reconciling ...")
			r.updateStatus(ctx, pulp, metav1.ConditionFalse, conditionType, "UpdatingFileStoragePVC", "Reconciling "+pulp.Name+"-file-storage PVC resource")
			r.recorder.Event(pulp, corev1.EventTypeNormal, "Updating", "Reconciling file storage PVC")
			err = r.Update(ctx, expected_pvc.(*corev1.PersistentVolumeClaim))
			if err != nil {
				log.Error(err, "Error trying to update the PVC object ... ")
				r.updateStatus(ctx, pulp, metav1.ConditionFalse, conditionType, "ErrorUpdatingFileStoragePVC", "Failed to reconcile "+pulp.Name+"-file-storage PVC resource")
				r.recorder.Event(pulp, corev1.EventTypeWarning, "Failed", "Failed to reconcile file storage PVC")
				return ctrl.Result{}, err
			}
			r.recorder.Event(pulp, corev1.EventTypeNormal, "Updated", "File storage PVC reconciled")
			return ctrl.Result{Requeue: true, RequeueAfter: time.Second}, nil
		}
	}

	// if .spec.admin_password_secret is not defined, operator will default to pulp-admin-password
	adminSecretName := pulp.Name + "-admin-password"
	if len(pulp.Spec.AdminPasswordSecret) > 1 {
		adminSecretName = pulp.Spec.AdminPasswordSecret
	}

	// update pulp CR with container_token_secret secret value
	if len(pulp.Spec.ContainerTokenSecret) == 0 {
		patch := client.MergeFrom(pulp.DeepCopy())
		pulp.Spec.ContainerTokenSecret = pulp.Name + "-container-auth"
		r.Patch(ctx, pulp, patch)
	}

	// define the k8s Deployment function based on k8s distribution and deployment type
	deploymentForPulpApi := initDeployment(API_DEPLOYMENT).deploy

	// list of pulp-api resources that should be provisioned
	resources := []ApiResource{
		// pulp-server secret
		{Definition: ResourceDefinition{Context: ctx, Type: &corev1.Secret{}, Name: pulp.Name + "-server", Alias: "Server", ConditionType: conditionType, Pulp: pulp}, Function: pulpServerSecret},
		// pulp-db-fields-encryption secret
		{ResourceDefinition{ctx, &corev1.Secret{}, pulp.Name + "-db-fields-encryption", "DBFieldsEncryption", conditionType, pulp}, pulpDBFieldsEncryptionSecret},
		// pulp-admin-password secret
		{ResourceDefinition{ctx, &corev1.Secret{}, adminSecretName, "AdminPassword", conditionType, pulp}, pulpAdminPasswordSecret},
		// pulp-container-auth secret
		{ResourceDefinition{ctx, &corev1.Secret{}, pulp.Spec.ContainerTokenSecret, "ContainerAuth", conditionType, pulp}, pulpContainerAuth},
		// pulp-api deployment
		{ResourceDefinition{ctx, &appsv1.Deployment{}, pulp.Name + "-api", "Api", conditionType, pulp}, deploymentForPulpApi},
		// pulp-api-svc service
		{ResourceDefinition{ctx, &corev1.Service{}, pulp.Name + "-api-svc", "Api", conditionType, pulp}, serviceForAPI},
	}

	// create pulp-api resources
	for _, resource := range resources {
		requeue, err := r.createPulpResource(resource.Definition, resource.Function)
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// update pulp CR admin-password secret with default name
	if err := r.updateCRField(ctx, pulp, "AdminPasswordSecret", pulp.Name+"-admin-password"); err != nil {
		return ctrl.Result{}, err
	}

	// Ensure the deployment spec is as expected
	apiDeployment := &appsv1.Deployment{}
	r.Get(ctx, types.NamespacedName{Name: pulp.Name + "-api", Namespace: pulp.Namespace}, apiDeployment)
	expected := deploymentForPulpApi(funcResources)
	if requeue, err := reconcileObject(funcResources, expected, apiDeployment, conditionType); err != nil || requeue {
		return ctrl.Result{Requeue: requeue}, err
	}

	// update pulp CR with default values
	if len(pulp.Spec.DBFieldsEncryptionSecret) == 0 {
		patch := client.MergeFrom(pulp.DeepCopy())
		pulp.Spec.DBFieldsEncryptionSecret = pulp.Name + "-db-fields-encryption"
		r.Patch(ctx, pulp, patch)
	}

	// Ensure the service spec is as expected
	apiSvc := &corev1.Service{}
	r.Get(ctx, types.NamespacedName{Name: pulp.Name + "-api-svc", Namespace: pulp.Namespace}, apiSvc)
	expectedSvc := serviceForAPI(funcResources)
	if requeue, err := reconcileObject(funcResources, expectedSvc, apiSvc, conditionType); err != nil || requeue {
		return ctrl.Result{Requeue: requeue}, err
	}

	// Ensure the secret data is as expected
	serverSecret := &corev1.Secret{}
	r.Get(ctx, types.NamespacedName{Name: pulp.Name + "-server", Namespace: pulp.Namespace}, serverSecret)
	expectedServerSecret := pulpServerSecret(funcResources)
	if requeue, err := reconcileObject(funcResources, expectedServerSecret, serverSecret, conditionType); err != nil || requeue {
		log.Info("Reprovisioning pulpcore-api pods to get the new settings ...")
		// when requeue==true it means the secret changed so we need to redeploy api and content pods to get the new settings.py
		r.restartPods(pulp, apiDeployment)
		contentDeployment := &appsv1.Deployment{}
		r.Get(ctx, types.NamespacedName{Name: pulp.Name + "-content", Namespace: pulp.Namespace}, contentDeployment)
		log.Info("Reprovisioning pulpcore-content pods to get the new settings ...")
		r.restartPods(pulp, contentDeployment)

		return ctrl.Result{Requeue: requeue}, err
	}

	return ctrl.Result{}, nil
}

// fileStoragePVC returns a PVC object
func fileStoragePVC(resources FunctionResources) client.Object {

	// Define the new PVC
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.Pulp.Name + "-file-storage",
			Namespace: resources.Pulp.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       resources.Pulp.Spec.DeploymentType + "-storage",
				"app.kubernetes.io/instance":   resources.Pulp.Spec.DeploymentType + "-storage-" + resources.Pulp.Name,
				"app.kubernetes.io/component":  "storage",
				"app.kubernetes.io/part-of":    resources.Pulp.Spec.DeploymentType,
				"app.kubernetes.io/managed-by": resources.Pulp.Spec.DeploymentType + "-operator",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceName(corev1.ResourceStorage): resource.MustParse(resources.Pulp.Spec.FileStorageSize),
				},
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.PersistentVolumeAccessMode(resources.Pulp.Spec.FileStorageAccessMode),
			},
			StorageClassName: &resources.Pulp.Spec.FileStorageClass,
		},
	}

	// Set Pulp instance as the owner and controller
	ctrl.SetControllerReference(resources.Pulp, pvc, resources.RepoManagerReconciler.Scheme)
	return pvc
}

// labelsForPulpApi returns the labels for selecting the resources
// belonging to the given pulp CR name.
func labelsForPulpApi(m *repomanagerpulpprojectorgv1beta2.Pulp) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       m.Spec.DeploymentType + "-api",
		"app.kubernetes.io/instance":   m.Spec.DeploymentType + "-api-" + m.Name,
		"app.kubernetes.io/component":  "api",
		"app.kubernetes.io/part-of":    m.Spec.DeploymentType,
		"app.kubernetes.io/managed-by": m.Spec.DeploymentType + "-operator",
		"app":                          "pulp-api",
		"pulp_cr":                      m.Name,
	}
}

// pulpServerSecret creates the pulp-server secret object which is used to
// populate the /etc/pulp/settings.py config file
func pulpServerSecret(resources FunctionResources) client.Object {

	var dbHost, dbPort, dbUser, dbPass, dbName, dbSSLMode string
	_, storageType := controllers.MultiStorageConfigured(resources.Pulp, "Pulp")

	// if there is no external database configuration get the databaseconfig from pulp-postgres-configuration secret
	if len(resources.Pulp.Spec.Database.ExternalDBSecret) == 0 {
		postgresConfigurationSecret := resources.Pulp.Name + "-postgres-configuration"
		if len(resources.Pulp.Spec.PostgresConfigurationSecret) > 0 { // [DEPRECATED] Temporarily adding to keep compatibility with ansible version.
			postgresConfigurationSecret = resources.Pulp.Spec.PostgresConfigurationSecret
		}

		resources.Logger.V(1).Info("Retrieving Postgres credentials from "+postgresConfigurationSecret+" secret", "Secret.Namespace", resources.Pulp.Namespace, "Secret.Name", resources.Pulp.Name)
		pgCredentials, err := resources.RepoManagerReconciler.retrieveSecretData(resources.Context, postgresConfigurationSecret, resources.Pulp.Namespace, true, "username", "password", "database", "port", "sslmode")
		if err != nil {
			resources.Logger.Error(err, "Secret Not Found!", "Secret.Namespace", resources.Pulp.Namespace, "Secret.Name", resources.Pulp.Name)
		}
		dbHost = resources.Pulp.Name + "-database-svc"
		dbPort = pgCredentials["port"]
		dbUser = pgCredentials["username"]
		dbPass = pgCredentials["password"]
		dbName = pgCredentials["database"]
		dbSSLMode = pgCredentials["sslmode"]
	} else {
		resources.Logger.V(1).Info("Retrieving Postgres credentials from "+resources.Pulp.Spec.Database.ExternalDBSecret+" secret", "Secret.Namespace", resources.Pulp.Namespace, "Secret.Name", resources.Pulp.Name)
		externalPostgresData := []string{"POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USERNAME", "POSTGRES_PASSWORD", "POSTGRES_DB_NAME", "POSTGRES_SSLMODE"}
		pgCredentials, err := resources.RepoManagerReconciler.retrieveSecretData(resources.Context, resources.Pulp.Spec.Database.ExternalDBSecret, resources.Pulp.Namespace, true, externalPostgresData...)
		if err != nil {
			resources.Logger.Error(err, "Secret Not Found!", "Secret.Namespace", resources.Pulp.Namespace, "Secret.Name", resources.Pulp.Name)
		}
		dbHost = pgCredentials["POSTGRES_HOST"]
		dbPort = pgCredentials["POSTGRES_PORT"]
		dbUser = pgCredentials["POSTGRES_USERNAME"]
		dbPass = pgCredentials["POSTGRES_PASSWORD"]
		dbName = pgCredentials["POSTGRES_DB_NAME"]
		dbSSLMode = pgCredentials["POSTGRES_SSLMODE"]
	}

	// Handling user facing URLs
	rootUrl := getRootURL(resources)

	// default settings.py configuration
	var pulp_settings = controllers.DotNotEditMessage + `
DB_ENCRYPTION_KEY = "/etc/pulp/keys/database_fields.symmetric.key"
GALAXY_COLLECTION_SIGNING_SERVICE = "ansible-default"
GALAXY_CONTAINER_SIGNING_SERVICE = "container-default"
ANSIBLE_API_HOSTNAME = "` + rootUrl + `"
ANSIBLE_CERTS_DIR = "/etc/pulp/keys/"
CONTENT_ORIGIN = "` + rootUrl + `"
DATABASES = {
	'default': {
		'HOST': '` + dbHost + `',
		'ENGINE': 'django.db.backends.postgresql_psycopg2',
		'NAME': '` + dbName + `',
		'USER': '` + dbUser + `',
		'PASSWORD': '` + dbPass + `',
		'PORT': '` + dbPort + `',
		'CONN_MAX_AGE': 0,
		'OPTIONS': { 'sslmode': '` + dbSSLMode + `' },
	}
}
GALAXY_FEATURE_FLAGS = {
	'execution_environments': 'True',
}
PRIVATE_KEY_PATH = "/etc/pulp/keys/container_auth_private_key.pem"
PUBLIC_KEY_PATH = "/etc/pulp/keys/container_auth_public_key.pem"
STATIC_ROOT = "/var/lib/operator/static/"
TOKEN_AUTH_DISABLED = False
TOKEN_SIGNATURE_ALGORITHM = "ES256"
`

	pulp_settings = pulp_settings + fmt.Sprintln("API_ROOT = \"/pulp/\"")

	// add cache settings
	if resources.Pulp.Spec.Cache.Enabled {

		var cacheHost, cachePort, cachePassword, cacheDB string

		// if there is no ExternalCacheSecret defined, we should
		// use the redis instance provided by the operator
		if len(resources.Pulp.Spec.Cache.ExternalCacheSecret) == 0 {
			if resources.Pulp.Spec.Cache.RedisPort == 0 {
				cachePort = strconv.Itoa(6379)
			} else {
				cachePort = strconv.Itoa(resources.Pulp.Spec.Cache.RedisPort)
			}
			cacheHost = resources.Pulp.Name + "-redis-svc." + resources.Pulp.Namespace
		} else {
			// retrieve the connection data from ExternalCacheSecret secret
			externalCacheData := []string{"REDIS_HOST", "REDIS_PORT", "REDIS_PASSWORD", "REDIS_DB"}
			externalCacheConfig, _ := resources.RepoManagerReconciler.retrieveSecretData(context.TODO(), resources.Pulp.Spec.Cache.ExternalCacheSecret, resources.Pulp.Namespace, true, externalCacheData...)
			cacheHost = externalCacheConfig["REDIS_HOST"]
			cachePort = externalCacheConfig["REDIS_PORT"]
			cachePassword = externalCacheConfig["REDIS_PASSWORD"]
			cacheDB = externalCacheConfig["REDIS_DB"]
		}

		cacheSettings := `CACHE_ENABLED = True
REDIS_HOST =  "` + cacheHost + `"
REDIS_PORT =  "` + cachePort + `"
REDIS_PASSWORD = "` + cachePassword + `"
REDIS_DB = "` + cacheDB + `"
`
		pulp_settings = pulp_settings + cacheSettings
	}

	// if an Azure Blob is defined in Pulp CR we should add the
	// credentials from azure secret into settings.py
	if storageType[0] == controllers.AzureObjType {
		resources.Logger.V(1).Info("Retrieving Azure data from " + resources.Pulp.Spec.ObjectStorageAzureSecret)
		storageData, err := resources.RepoManagerReconciler.retrieveSecretData(resources.Context, resources.Pulp.Spec.ObjectStorageAzureSecret, resources.Pulp.Namespace, true, "azure-account-name", "azure-account-key", "azure-container", "azure-container-path", "azure-connection-string")
		if err != nil {
			resources.Logger.Error(err, "Secret Not Found!", "Secret.Namespace", resources.Pulp.Namespace, "Secret.Name", resources.Pulp.Spec.ObjectStorageAzureSecret)
			return &corev1.Secret{}
		}
		pulp_settings = pulp_settings + `AZURE_CONNECTION_STRING = '` + storageData["azure-connection-string"] + `'
AZURE_LOCATION = '` + storageData["azure-container-path"] + `'
AZURE_ACCOUNT_NAME = '` + storageData["azure-account-name"] + `'
AZURE_ACCOUNT_KEY = '` + storageData["azure-account-key"] + `'
AZURE_CONTAINER = '` + storageData["azure-container"] + `'
AZURE_URL_EXPIRATION_SECS = 60
AZURE_OVERWRITE_FILES = True
DEFAULT_FILE_STORAGE = "storages.backends.azure_storage.AzureStorage"
`
	}

	// if a S3 is defined in Pulp CR we should add the
	// credentials from aws secret into settings.py
	if storageType[0] == controllers.S3ObjType {
		resources.Logger.V(1).Info("Retrieving S3 data from " + resources.Pulp.Spec.ObjectStorageS3Secret)
		storageData, err := resources.RepoManagerReconciler.retrieveSecretData(resources.Context, resources.Pulp.Spec.ObjectStorageS3Secret, resources.Pulp.Namespace, true, "s3-access-key-id", "s3-secret-access-key", "s3-bucket-name")
		if err != nil {
			resources.Logger.Error(err, "Secret Not Found!", "Secret.Namespace", resources.Pulp.Namespace, "Secret.Name", resources.Pulp.Spec.ObjectStorageS3Secret)
			return &corev1.Secret{}
		}

		optionalKey, _ := resources.RepoManagerReconciler.retrieveSecretData(resources.Context, resources.Pulp.Spec.ObjectStorageS3Secret, resources.Pulp.Namespace, false, "s3-endpoint", "s3-region")
		if len(optionalKey["s3-endpoint"]) == 0 && len(optionalKey["s3-region"]) == 0 {
			resources.Logger.Error(err, "Either s3-endpoint or s3-region needs to be specified", "Secret.Namespace", resources.Pulp.Namespace, "Secret.Name", resources.Pulp.Spec.ObjectStorageS3Secret)
		}
		if len(optionalKey["s3-endpoint"]) > 0 {
			pulp_settings = pulp_settings + fmt.Sprintf("AWS_S3_ENDPOINT_URL = \"%v\"\n", optionalKey["s3-endpoint"])
		}
		if len(optionalKey["s3-region"]) > 0 {
			pulp_settings = pulp_settings + fmt.Sprintf("AWS_S3_REGION_NAME = \"%v\"\n", optionalKey["s3-region"])
		}

		pulp_settings = pulp_settings + `AWS_ACCESS_KEY_ID = '` + storageData["s3-access-key-id"] + `'
AWS_SECRET_ACCESS_KEY = '` + storageData["s3-secret-access-key"] + `'
AWS_STORAGE_BUCKET_NAME = '` + storageData["s3-bucket-name"] + `'
AWS_DEFAULT_ACL = "@none None"
S3_USE_SIGV4 = True
AWS_S3_SIGNATURE_VERSION = "s3v4"
AWS_S3_ADDRESSING_STYLE = "path"
DEFAULT_FILE_STORAGE = "storages.backends.s3boto3.S3Boto3Storage"
MEDIA_ROOT = ""
`
	}

	// configure settings.py with keycloak integration variables
	if len(resources.Pulp.Spec.SSOSecret) > 0 {
		resources.RepoManagerReconciler.ssoConfig(resources.Context, resources.Pulp, &pulp_settings)
	}

	// configure TOKEN_SERVER based on ingress_type
	tokenServer := "http://" + resources.Pulp.Name + "-api-svc." + resources.Pulp.Namespace + ".svc.cluster.local:24817/token/"
	if resources.Pulp.Spec.IngressType == "route" {
		tokenServer = rootUrl + "/token/"
	} else if resources.Pulp.Spec.IngressType == "ingress" {
		proto := "http"
		if len(resources.Pulp.Spec.IngressTLSSecret) > 0 {
			proto = "https"
		}
		hostname := resources.Pulp.Spec.IngressHost
		if len(resources.Pulp.Spec.Hostname) > 0 { // [DEPRECATED] Temporarily adding to keep compatibility with ansible version.
			hostname = resources.Pulp.Spec.Hostname
		}
		tokenServer = proto + "://" + hostname + "/token/"
	}
	pulp_settings = pulp_settings + fmt.Sprintln("TOKEN_SERVER = \""+tokenServer+"\"")

	// add custom settings to the secret
	pulp_settings = addCustomPulpSettings(resources.Pulp, pulp_settings)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.Pulp.Name + "-server",
			Namespace: resources.Pulp.Namespace,
		},
		StringData: map[string]string{
			"settings.py": pulp_settings,
		},
	}

	// Set Pulp instance as the owner and controller
	ctrl.SetControllerReference(resources.Pulp, sec, resources.RepoManagerReconciler.Scheme)
	return sec
}

// pulp-db-fields-encryption secret
func pulpDBFieldsEncryptionSecret(resources FunctionResources) client.Object {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.Pulp.Name + "-db-fields-encryption",
			Namespace: resources.Pulp.Namespace,
		},
		StringData: map[string]string{
			"database_fields.symmetric.key": createFernetKey(),
		},
	}
	return sec
}

// pulp-admin-passowrd
func pulpAdminPasswordSecret(resources FunctionResources) client.Object {

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.Pulp.Name + "-admin-password",
			Namespace: resources.Pulp.Namespace,
		},
		StringData: map[string]string{
			"password": createPwd(32),
		},
	}
	ctrl.SetControllerReference(resources.Pulp, sec, resources.RepoManagerReconciler.Scheme)

	return sec
}

// pulp-container-auth
func pulpContainerAuth(resources FunctionResources) client.Object {

	privKey, pubKey := genTokenAuthKey()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.Pulp.Spec.ContainerTokenSecret,
			Namespace: resources.Pulp.Namespace,
		},
		StringData: map[string]string{
			"container_auth_private_key.pem": privKey,
			"container_auth_public_key.pem":  pubKey,
		},
	}
}

// serviceForAPI returns a service object for pulp-api
func serviceForAPI(resources FunctionResources) client.Object {

	svc := serviceAPIObject(resources.Pulp.Name, resources.Pulp.Namespace, resources.Pulp.Spec.DeploymentType)

	// Set Pulp instance as the owner and controller
	ctrl.SetControllerReference(resources.Pulp, svc, resources.RepoManagerReconciler.Scheme)
	return svc
}

func serviceAPIObject(name, namespace, deployment_type string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-api-svc",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       deployment_type + "-api",
				"app.kubernetes.io/instance":   deployment_type + "-api-" + name,
				"app.kubernetes.io/component":  "api",
				"app.kubernetes.io/part-of":    deployment_type,
				"app.kubernetes.io/managed-by": deployment_type + "-operator",
				"app":                          "pulp-api",
				"pulp_cr":                      name,
			},
		},
		Spec: serviceAPISpec(name, namespace, deployment_type),
	}
}

// api service spec
func serviceAPISpec(name, namespace, deployment_type string) corev1.ServiceSpec {

	serviceInternalTrafficPolicyCluster := corev1.ServiceInternalTrafficPolicyType("Cluster")
	ipFamilyPolicyType := corev1.IPFamilyPolicyType("SingleStack")
	serviceAffinity := corev1.ServiceAffinity("None")
	servicePortProto := corev1.Protocol("TCP")
	targetPort := intstr.IntOrString{IntVal: 24817}
	serviceType := corev1.ServiceType("ClusterIP")

	return corev1.ServiceSpec{
		InternalTrafficPolicy: &serviceInternalTrafficPolicyCluster,
		IPFamilies:            []corev1.IPFamily{"IPv4"},
		IPFamilyPolicy:        &ipFamilyPolicyType,
		Ports: []corev1.ServicePort{{
			Name:       "api-24817",
			Port:       24817,
			Protocol:   servicePortProto,
			TargetPort: targetPort,
		}},
		Selector: map[string]string{
			"app.kubernetes.io/name":       deployment_type + "-api",
			"app.kubernetes.io/instance":   deployment_type + "-api-" + name,
			"app.kubernetes.io/component":  "api",
			"app.kubernetes.io/part-of":    deployment_type,
			"app.kubernetes.io/managed-by": deployment_type + "-operator",
			"app":                          "pulp-api",
			"pulp_cr":                      name,
		},
		SessionAffinity:          serviceAffinity,
		Type:                     serviceType,
		PublishNotReadyAddresses: true,
	}
}

// storageClassProvided returns true if a StorageClass is provided in Pulp CR
func storageClassProvided(pulp *repomanagerpulpprojectorgv1beta2.Pulp) bool {
	_, storageType := controllers.MultiStorageConfigured(pulp, "Pulp")
	return storageType[0] == controllers.SCNameType
}
