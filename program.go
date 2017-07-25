package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/Azure/azure-sdk-for-go/arm/keyvault"
	"github.com/Azure/azure-sdk-for-go/arm/resources/subscriptions"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/satori/uuid"
)

var resourceGroup string
var environment = azure.PublicCloud
var userTenantID uuid.UUID

func main() {
	var err error
	var clientID uuid.UUID
	clientID, err = uuid.FromString("04b07795-8ddb-461a-bbee-02f9e1bf7b46")

	var userSubscription string

	if err != nil {
		fmt.Fprint(os.Stderr, err)
		return
	}

	var token *adal.Token
	token, err = authenticate(clientID) // This is the client ID for the Azure CLI. It was chosen for its public well-known status.
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	authorizer := autorest.NewBearerAuthorizer(token)

	var subscriptionCache []subscriptions.Subscription
	rawSubscriptions, errs := getSubscriptions(authorizer)

	for sub := range rawSubscriptions {
		subscriptionCache = append(subscriptionCache, sub)
	}
	if err = <-errs; err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	if subCount := len(subscriptionCache); subCount == 0 {
		fmt.Fprintln(os.Stderr, "No subscriptions found for this user in this tenant.")
		return
	} else if subCount == 1 {
		userSubscription = *subscriptionCache[0].SubscriptionID
	} else {
		fmt.Println("Please select a subscription:")
		for i, sub := range subscriptionCache {
			fmt.Printf("\t%2d) %s\n", i, *sub.DisplayName)
		}
		fmt.Print("Selection: ")
		var selected int
		fmt.Scanf("%d", &selected)

		userSubscription = *subscriptionCache[selected].SubscriptionID
	}

	vaultClient := keyvault.NewVaultsClient(userSubscription)
	vaultClient.Authorizer = authorizer

	var results keyvault.ResourceListResult
	results, err = vaultClient.List("", to.Int32Ptr(30))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	fmt.Println("Resources Found:")
	for _, r := range *results.Value {
		fmt.Println("\t", *r.Name)
	}
}

func init() {
	flag.StringVar(&resourceGroup, "rg", "", "The name of the resource group to scrape for key vaults.")
	flag.Parse()
}

// authenticate gets an authorization token to allow clients to access Azure assets.
func authenticate(clientID uuid.UUID) (token *adal.Token, err error) {
	authClient := autorest.NewClientWithUserAgent("issue696 repro")
	var deviceCode *adal.DeviceCode
	var config *adal.OAuthConfig

	config, err = adal.NewOAuthConfig(environment.ActiveDirectoryEndpoint, "common")
	if err != nil {
		return
	}

	deviceCode, err = adal.InitiateDeviceAuth(&authClient, *config, clientID.String(), environment.ServiceManagementEndpoint)
	if err != nil {
		return
	}

	_, err = fmt.Println(*deviceCode.Message)
	if err != nil {
		return
	}

	token, err = adal.WaitForUserCompletion(&authClient, deviceCode)
	if err != nil {
		return
	}

	var tenantCache []string
	tenants, tenantErrs := getTenants(autorest.NewBearerAuthorizer(token))
	for tenant := range tenants {
		tenantCache = append(tenantCache, *tenant.TenantID)
	}
	err = <-tenantErrs
	if err != nil {
		return
	}

	if len(tenantCache) == 1 {
		userTenantID, err = uuid.FromString(tenantCache[0])
	} else {
		err = errors.New("zero or multiple tenants associated with this account")
		return
	}

	config, err = adal.NewOAuthConfig(environment.ActiveDirectoryEndpoint, userTenantID.String())

	var spt *adal.ServicePrincipalToken
	spt, err = adal.NewServicePrincipalTokenFromManualToken(*config, clientID.String(), environment.ResourceManagerEndpoint, *token)
	if err != nil {
		token = nil
		return
	}

	token = &spt.Token
	return
}

func getTenants(authorizer autorest.Authorizer) (<-chan subscriptions.TenantIDDescription, <-chan error) {
	results, errs := make(chan subscriptions.TenantIDDescription), make(chan error, 1)
	go func() {
		var err error
		var fetchTenantUpdater sync.Once

		defer close(results)
		defer close(errs)

		tenantClient := subscriptions.NewTenantsClient()
		tenantClient.Authorizer = authorizer

		var fetchTenants func() (subscriptions.TenantListResult, error)
		fetchTenants = tenantClient.List

		var tenantChunk subscriptions.TenantListResult
		for {
			tenantChunk, err = fetchTenants()
			if err != nil {
				errs <- err
				return
			}

			for _, tenant := range *tenantChunk.Value {
				results <- tenant
			}

			if tenantChunk.NextLink == nil {
				break
			}
			fetchTenantUpdater.Do(func() {
				fetchTenants = func() (subscriptions.TenantListResult, error) {
					return tenantClient.ListNextResults(tenantChunk)
				}
			})
		}

	}()
	return results, errs
}

func getSubscriptions(authorizer autorest.Authorizer) (<-chan subscriptions.Subscription, <-chan error) {
	results, errs := make(chan subscriptions.Subscription), make(chan error, 1)

	go func() {
		var err error
		var fetchSubscriptionsUpdater sync.Once

		defer close(results)
		defer close(errs)

		client := subscriptions.NewGroupClient()
		client.Authorizer = authorizer

		var fetchSubscriptions func() (subscriptions.ListResult, error)
		fetchSubscriptions = client.List

		for {
			var subscriptionChunk subscriptions.ListResult

			subscriptionChunk, err = fetchSubscriptions()
			if err != nil {
				errs <- err
				return
			}

			for _, subscription := range *subscriptionChunk.Value {
				results <- subscription
			}

			if subscriptionChunk.NextLink == nil {
				break
			}

			fetchSubscriptionsUpdater.Do(func() {
				fetchSubscriptions = func() (subscriptions.ListResult, error) {
					return client.ListNextResults(subscriptionChunk)
				}
			})
		}
	}()

	return results, errs
}
