package compassconnection

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kyma-incubator/compass/components/director/pkg/str"

	"github.com/kyma-incubator/compass/components/director/pkg/correlation"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/kyma-project/kyma/components/compass-runtime-agent/internal/compass/cache"

	"github.com/kyma-incubator/compass/components/director/pkg/graphql"

	"github.com/kyma-project/kyma/components/compass-runtime-agent/internal/compass/director"

	"github.com/kyma-project/kyma/components/compass-runtime-agent/internal/kyma"

	"github.com/kyma-project/kyma/components/compass-runtime-agent/internal/apperrors"

	"github.com/kyma-project/kyma/components/compass-runtime-agent/internal/certificates"

	"github.com/stretchr/testify/assert"

	certsMocks "github.com/kyma-project/kyma/components/compass-runtime-agent/internal/certificates/mocks"
	directorMocks "github.com/kyma-project/kyma/components/compass-runtime-agent/internal/compass/director/mocks"
	compassMocks "github.com/kyma-project/kyma/components/compass-runtime-agent/internal/compass/mocks"
	"github.com/kyma-project/kyma/components/compass-runtime-agent/internal/config"
	configMocks "github.com/kyma-project/kyma/components/compass-runtime-agent/internal/config/mocks"
	kymaMocks "github.com/kyma-project/kyma/components/compass-runtime-agent/internal/kyma/mocks"
	kymaModel "github.com/kyma-project/kyma/components/compass-runtime-agent/internal/kyma/model"

	"github.com/stretchr/testify/mock"

	gqlschema "github.com/kyma-incubator/compass/components/connector/pkg/graphql/externalschema"

	connectorMocks "github.com/kyma-project/kyma/components/compass-runtime-agent/internal/compass/connector/mocks"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kyma-project/kyma/components/compass-runtime-agent/pkg/apis/compass/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"
)

const (
	compassConnectionName = "compass-connection"
	token                 = "token"
	runtimeId             = "abcd-efgh-ijkl"

	syncPeriod            = 2 * time.Second
	minimalConfigSyncTime = 4 * time.Second

	checkInterval = 2 * time.Second
	testTimeout   = 20 * time.Second
)

var (
	connectorTokenHeaders = map[string]string{
		ConnectorTokenHeader: token,
	}
	connectorTokenHeadersFunc = mock.MatchedBy(func(input map[string]string) bool {
		for k, v := range connectorTokenHeaders {
			if input[k] != v {
				return false
			}
		}
		return true
	})
	nilHeaders map[string]string

	connectorURL            = "https://connector.com"
	directorURL             = "https://director.com"
	certSecuredConnectorURL = "https://cert-connector.com"

	connectorConfigurationResponse = gqlschema.Configuration{
		Token: &gqlschema.Token{Token: token},
		CertificateSigningRequestInfo: &gqlschema.CertificateSigningRequestInfo{
			Subject:      "O=Org,OU=OrgUnit,L=locality,ST=province,C=DE,CN=test",
			KeyAlgorithm: "rsa2048",
		},
		ManagementPlaneInfo: &gqlschema.ManagementPlaneInfo{
			DirectorURL:                    &directorURL,
			CertificateSecuredConnectorURL: &certSecuredConnectorURL,
		},
	}

	connectionConfig = config.ConnectionConfig{
		Token:        token,
		ConnectorURL: connectorURL,
	}

	runtimeConfig = config.RuntimeConfig{RuntimeId: runtimeId}

	runtimeURLsConfig = director.RuntimeURLsConfig{
		EventsURL:  "https://gateway.kyma.local",
		ConsoleURL: "https://console.kyma.local",
	}

	runtimeLabels = graphql.Labels{
		"events":  runtimeURLsConfig.EventsURL,
		"console": runtimeURLsConfig.ConsoleURL,
	}

	kymaModelApps = []kymaModel.Application{{Name: "App-1", ID: "abcd-efgh"}}
	testCtx       = correlation.SaveCorrelationIDHeaderToContext(context.Background(), str.Ptr(correlation.RequestIDHeaderKey), str.Ptr("test-correlation-id-123"))

	operationResults    = []kyma.Result{{ApplicationName: "App-1", ApplicationID: "abcd-efgh", Operation: kyma.Create}}
	requestIDCtxMatcher = mock.MatchedBy(func(ctx context.Context) bool {
		if val := ctx.Value(correlation.HeadersContextKey); val != nil {
			if h, ok := val.(correlation.Headers); ok {
				_, res := h[correlation.RequestIDHeaderKey]
				return res
			}
		}
		return false
	})
)

func TestCompassConnectionController(t *testing.T) {

	syncPeriodTime := syncPeriod
	ctrlManager, err := manager.New(cfg, manager.Options{SyncPeriod: &syncPeriodTime})
	require.NoError(t, err)

	// Credentials manager
	credentialsManagerMock := &certsMocks.Manager{}
	credentialsManagerMock.On("PreserveCredentials", mock.AnythingOfType("certificates.Credentials")).Run(func(args mock.Arguments) {
		credentials, ok := args[0].(certificates.Credentials)
		assert.True(t, ok)
		assert.NotEmpty(t, credentials)
	}).Return(nil)
	credentialsManagerMock.On("CredentialsExist").Return(true, nil)

	// Config provider
	configProviderMock := configProviderMock()
	// Connector clients
	tokensConnectorClientMock := connectorTokensClientMock(requestIDCtxMatcher)
	certsConnectorClientMock := connectorCertClientMock(requestIDCtxMatcher)
	// Director config client
	configurationClientMock := &directorMocks.DirectorClient{}
	configurationClientMock.On("FetchConfiguration", requestIDCtxMatcher).Return(kymaModelApps, graphql.Labels{}, nil)
	configurationClientMock.On("SetURLsLabels", requestIDCtxMatcher, runtimeURLsConfig, graphql.Labels{}).Return(runtimeLabels, nil)
	// Director proxy configurator
	directorProxyConfiguratorMock := &directorMocks.ProxyConfigurator{}
	directorProxyConfiguratorMock.On("SetURLAndCerts", mock.AnythingOfType("cache.ConnectionData")).Return(nil)
	// Clients provider
	clientsProviderMock := clientsProviderMock(configurationClientMock, tokensConnectorClientMock, certsConnectorClientMock)
	// Sync service
	synchronizationServiceMock := &kymaMocks.Service{}
	synchronizationServiceMock.On("Apply", kymaModelApps).Return(operationResults, nil)

	connectionDataCache := cache.NewConnectionDataCache()
	connectionDataCache.AddSubscriber(func(data cache.ConnectionData) error {
		assert.NotEmpty(t, data.Certificate)
		assert.Equal(t, certSecuredConnectorURL, data.ConnectorURL)
		assert.Equal(t, directorURL, data.DirectorURL)
		return nil
	})
	connectionDataCache.AddSubscriber(directorProxyConfiguratorMock.SetURLAndCerts)

	var baseDependencies = DependencyConfig{
		K8sConfig:         cfg,
		ControllerManager: ctrlManager,

		ClientsProvider:              clientsProviderMock,
		CredentialsManager:           credentialsManagerMock,
		SynchronizationService:       synchronizationServiceMock,
		ConfigProvider:               configProviderMock,
		CertValidityRenewalThreshold: 0.3,
		MinimalCompassSyncTime:       minimalConfigSyncTime,
		ConnectionDataCache:          connectionDataCache,

		RuntimeURLsConfig: runtimeURLsConfig,
	}

	supervisor, err := baseDependencies.InitializeController()
	require.NoError(t, err)

	defer func() {
		err := compassConnectionCRClient.Delete(context.Background(), compassConnectionName, v1.DeleteOptions{})
		if err != nil {
			t.Logf("error while deleting Compass Connection: %s", err.Error())
		}
	}()
	cancelFunc, _ := StartTestManager(t, ctrlManager)
	defer cancelFunc()

	connection, err := supervisor.InitializeCompassConnection(testCtx)
	require.NoError(t, err)
	assert.NotEmpty(t, connection)

	t.Run("Compass Connection should be synchronized after few seconds", func(t *testing.T) {
		// when
		waitForResynchronization()

		// then
		assertCompassConnectionState(t, v1alpha1.Synchronized)
		assertConnectionStatusSet(t)
		assertManagementInfoSetInCR(t)

		mock.AssertExpectationsForObjects(t,
			tokensConnectorClientMock,
			configurationClientMock,
			synchronizationServiceMock,
			clientsProviderMock,
			configProviderMock,
			credentialsManagerMock)
		certsConnectorClientMock.AssertCalled(t, "Configuration", requestIDCtxMatcher, nilHeaders)
		certsConnectorClientMock.AssertNotCalled(t, "SignCSR", requestIDCtxMatcher, mock.AnythingOfType("string"), nilHeaders)
	})

	t.Run("Compass Connection should be reinitialized if deleted", func(t *testing.T) {
		// given
		err := compassConnectionCRClient.Delete(context.Background(), compassConnectionName, v1.DeleteOptions{})
		require.NoError(t, err)

		// then
		err = waitFor(checkInterval, testTimeout, func() bool {
			return isConnectionInState(v1alpha1.Synchronized)
		})
		require.NoError(t, err)
		assertConnectionStatusSet(t)
		assertManagementInfoSetInCR(t)

		mock.AssertExpectationsForObjects(t,
			tokensConnectorClientMock,
			configurationClientMock,
			synchronizationServiceMock,
			clientsProviderMock,
			configProviderMock,
			credentialsManagerMock)
		certsConnectorClientMock.AssertCalled(t, "Configuration", requestIDCtxMatcher, nilHeaders)
		certsConnectorClientMock.AssertNotCalled(t, "SignCSR", requestIDCtxMatcher, mock.AnythingOfType("string"), nilHeaders)
	})

	t.Run("Should not reinitialized connection if connection is in Synchronized state", func(t *testing.T) {
		// when
		credentialsManagerMock.On("GetClientCredentials").Return(credentials.ClientCredentials, nil)

		connection, err := supervisor.InitializeCompassConnection(context.Background())

		// then
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.Synchronized, connection.Status.State)
	})

	t.Run("Should renew certificate if RefreshCredentialsNow set to true", func(t *testing.T) {
		// given
		connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
		require.NoError(t, err)

		connectedConnection.Spec.RefreshCredentialsNow = true

		// when
		connectedConnection, err = compassConnectionCRClient.Update(context.Background(), connectedConnection, v1.UpdateOptions{})
		require.NoError(t, err)

		err = waitFor(checkInterval, testTimeout, func() bool {
			fakeT := &testing.T{}
			called := certsConnectorClientMock.AssertCalled(fakeT, "SignCSR", requestIDCtxMatcher, mock.AnythingOfType("string"), nilHeaders)
			return called
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.Synchronized))

		assertCertificateRenewed(t)
		assertManagementInfoSetInCR(t)
		certsConnectorClientMock.AssertCalled(t, "SignCSR", requestIDCtxMatcher, mock.AnythingOfType("string"), nilHeaders)
	})

	t.Run("Compass Connection should be in MetadataUpdateFailed state if failed to set labels on Runtime", func(t *testing.T) {
		// given
		clearMockCalls(&configurationClientMock.Mock)
		configurationClientMock.On("FetchConfiguration", requestIDCtxMatcher).Return(kymaModelApps, graphql.Labels{}, nil)
		configurationClientMock.On("SetURLsLabels", requestIDCtxMatcher, runtimeURLsConfig, graphql.Labels{}).Return(nil, apperrors.Internal("error"))

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&configurationClientMock.Mock, "SetURLsLabels", requestIDCtxMatcher, runtimeURLsConfig, graphql.Labels{})
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.MetadataUpdateFailed))
		assertManagementInfoSetInCR(t)

		clearMockCalls(&configurationClientMock.Mock)
		// restore previous director mock configuration to not interfere with other tests
		configurationClientMock.On("FetchConfiguration", requestIDCtxMatcher).Return(kymaModelApps, graphql.Labels{}, nil)
		configurationClientMock.On("SetURLsLabels", requestIDCtxMatcher, runtimeURLsConfig, graphql.Labels{}).Return(runtimeLabels, nil)
	})

	t.Run("Compass Connection should be in ResourceApplicationFailed state if failed to apply resources", func(t *testing.T) {
		// given
		clearMockCalls(&synchronizationServiceMock.Mock)
		synchronizationServiceMock.On("Apply", kymaModelApps).Return(nil, apperrors.Internal("error"))

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&synchronizationServiceMock.Mock, "Apply", kymaModelApps)
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.ResourceApplicationFailed))
		assertManagementInfoSetInCR(t)
		assertSynchronizationStatusError(t)

		// restore previous sync service mock configuration to not interfere with other tests
		clearMockCalls(&synchronizationServiceMock.Mock)
		synchronizationServiceMock.On("Apply", kymaModelApps).Return(operationResults, nil)
	})

	t.Run("Compass Connection should be in SynchronizationFailed state if failed to fetch configuration from Director", func(t *testing.T) {
		// given
		clearMockCalls(&configurationClientMock.Mock)
		configurationClientMock.On("FetchConfiguration", requestIDCtxMatcher).Return(nil, nil, errors.New("error"))

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&configurationClientMock.Mock, "FetchConfiguration", requestIDCtxMatcher)
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.SynchronizationFailed))
		assertManagementInfoSetInCR(t)
		assertSynchronizationStatusError(t)

		// restore previous director mock configuration to not interfere with other tests
		clearMockCalls(&configurationClientMock.Mock)
		configurationClientMock.On("FetchConfiguration", requestIDCtxMatcher).Return(kymaModelApps, graphql.Labels{}, nil)
		configurationClientMock.On("SetURLsLabels", requestIDCtxMatcher, runtimeURLsConfig, graphql.Labels{}).Return(runtimeLabels, nil)
	})

	// Control path A in controller - fetching config from Connector fails during connection initialisation
	t.Run("Compass Connection Controller should skip all attempts of Connection Initialisation if failed to fetch configuration from Connector AND minimalConfigSyncTime HAS NOT passed", func(t *testing.T) {
		// given
		badConnectorMock := &connectorMocks.Client{}
		badConnectorMock.On("Configuration", requestIDCtxMatcher, connectorTokenHeadersFunc).After(1*time.Second).Return(gqlschema.Configuration{}, errors.New("Failed to get configuration during initialisation. Timeout!!!"))

		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(badConnectorMock, nil)

		//this will enforce immediate connection reinitialisation. Then connection will be set to v1alpha1.ConnectionFailed
		err := compassConnectionCRClient.Delete(context.Background(), compassConnectionName, v1.DeleteOptions{})
		require.NoError(t, err)

		// when
		// 1 second for timeout
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&badConnectorMock.Mock, "Configuration", requestIDCtxMatcher, connectorTokenHeadersFunc)
		})
		require.NoError(t, err)

		err = waitFor(checkInterval, testTimeout, func() bool {
			return isConnectionInState(v1alpha1.ConnectionFailed)
		})
		require.NoError(t, err)
		assertConnectionStatusError(t)

		// Now wait for 2 seconds. Reconcile loop should run in the background.
		// Since minimalConfigSyncTime (4s) not pass all calls to connector should be skipped.
		time.Sleep(2 * time.Second)

		require.Equal(t, true, isConnectionInState(v1alpha1.ConnectionFailed))
		assertConnectionStatusError(t)

		badConnectorMock.AssertNumberOfCalls(t, "Configuration", 1)

		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetDirectorClient", runtimeConfig).Return(configurationClientMock, nil)
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(certsConnectorClientMock, nil)
		clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(tokensConnectorClientMock, nil)
	})

	t.Run("Compass Connection Controller should resume attempts of Connection Initialisation if failed to fetch configuration from Connector AND minimalConfigSyncTime HAS passed", func(t *testing.T) {
		// given
		badConnectorMock := &connectorMocks.Client{}
		badConnectorMock.On("Configuration", requestIDCtxMatcher, connectorTokenHeadersFunc).After(1*time.Second).Return(gqlschema.Configuration{}, errors.New("Failed to get configuration during initialisation. Timeout!!!"))

		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(badConnectorMock, nil)

		// this will enforce connection reinitialisation. Then connection will be set to v1alpha1.ConnectionFailed
		err := compassConnectionCRClient.Delete(context.Background(), compassConnectionName, v1.DeleteOptions{})
		require.NoError(t, err)

		// when
		// 1 second for timeout
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&badConnectorMock.Mock, "Configuration", requestIDCtxMatcher, connectorTokenHeadersFunc)
		})
		require.NoError(t, err)

		err = waitFor(checkInterval, testTimeout, func() bool {
			return isConnectionInState(v1alpha1.ConnectionFailed)
		})
		require.NoError(t, err)
		assertConnectionStatusError(t)

		// Now wait for minimalConfigSyncTime + 1s. Reconcile loop should run in the background.
		// After minimalConfigSyncTime passed calls to director can be resumed.
		time.Sleep(minimalConfigSyncTime + 2*time.Second)

		require.Equal(t, true, isConnectionInState(v1alpha1.ConnectionFailed))
		assertConnectionStatusError(t)

		badConnectorMock.AssertNumberOfCalls(t, "Configuration", 2)

		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetDirectorClient", runtimeConfig).Return(configurationClientMock, nil)
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(certsConnectorClientMock, nil)
		clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(tokensConnectorClientMock, nil)
	})

	// Control path B in controller - fetching config from Connector fails during connection maintenance
	t.Run("Compass Connection Controller should skip all attempts of Connection Maintenance if failed to fetch configuration from Connector AND minimalConfigSyncTime HAS NOT passed", func(t *testing.T) {
		// given
		assertRecreateSynchronizedConnection(t)
		badConnectorMock := &connectorMocks.Client{}
		badConnectorMock.On("Configuration", requestIDCtxMatcher, nilHeaders).After(1*time.Second).Return(gqlschema.Configuration{}, errors.New("Failed to get configuration during connection maintenance. Timeout!!!"))

		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(badConnectorMock, nil)

		// when
		// 2 seconds wait for call + 1 second for timeout
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&badConnectorMock.Mock, "Configuration", requestIDCtxMatcher, nilHeaders)
		})
		require.NoError(t, err)

		// Now wait for a second. Reconcile loop should run in the background.
		// Since minimalConfigSyncTime (4s) not pass all calls to connector should be skipped.
		time.Sleep(2 * time.Second)

		require.Equal(t, true, isConnectionInState(v1alpha1.ConnectionMaintenanceFailed))
		assertConnectionStatusError(t)
		badConnectorMock.AssertNumberOfCalls(t, "Configuration", 1)

		// restore previous connector mock configuration to not interfere with other tests
		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetDirectorClient", runtimeConfig).Return(configurationClientMock, nil)
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(certsConnectorClientMock, nil)
		clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(tokensConnectorClientMock, nil)
	})

	t.Run("Compass Connection Controller should resume attempts of Connection maintenance if failed to fetch configuration from Connector AND minimalConfigSyncTime HAS passed", func(t *testing.T) {
		// given
		assertRecreateSynchronizedConnection(t)
		badConnectorMock := &connectorMocks.Client{}
		badConnectorMock.On("Configuration", requestIDCtxMatcher, nilHeaders).After(1*time.Second).Return(gqlschema.Configuration{}, errors.New("Failed to get configuration during connection maintenance. Timeout!!!"))

		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(badConnectorMock, nil)

		// when
		// 2 seconds wait for call + 1 second for timeout
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&badConnectorMock.Mock, "Configuration", requestIDCtxMatcher, nilHeaders)
		})
		require.NoError(t, err)

		// Now wait for a minimalConfigSyncTime + 1s. Reconcile loop should run in the background.
		// After minimalConfigSyncTime passed calls to director can be resumed.
		time.Sleep(minimalConfigSyncTime + 2*time.Second)

		require.NoError(t, waitForResourceUpdate(v1alpha1.ConnectionMaintenanceFailed))
		assertConnectionStatusError(t)
		badConnectorMock.AssertNumberOfCalls(t, "Configuration", 2)

		// restore previous connector mock configuration to not interfere with other tests
		clearMockCalls(&clientsProviderMock.Mock)
		clientsProviderMock.On("GetDirectorClient", runtimeConfig).Return(configurationClientMock, nil)
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(certsConnectorClientMock, nil)
		clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(tokensConnectorClientMock, nil)
	})

	t.Run("Compass Connection Controller should wait at least minimalConfigSyncTime to retry fetch configuration from Director", func(t *testing.T) {
		// given
		assertRecreateSynchronizedConnection(t)
		require.Equal(t, true, isConnectionInState(v1alpha1.Synchronized))

		clearMockCalls(&configurationClientMock.Mock)
		configurationClientMock.On("FetchConfiguration", requestIDCtxMatcher).After(1*time.Second).Return(nil, nil, errors.New("Director timeout error!!!"))

		// ensure stability
		connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
		minuteBefore := v1.NewTime(time.Now().Add(time.Minute * -1))
		connectedConnection.Status.ConnectionStatus.LastSync = minuteBefore
		connectedConnection.Status.SynchronizationStatus.LastAttempt = minuteBefore
		connectedConnection, err = compassConnectionCRClient.Update(context.Background(), connectedConnection, v1.UpdateOptions{})
		require.NoError(t, err)

		//t.Logf("go back in time for the first time")
		// when
		// 1 seconds wait for call + 1 second for timeout
		err = waitFor(time.Second, testTimeout, func() bool {
			return mockFunctionCalled(&configurationClientMock.Mock, "FetchConfiguration", requestIDCtxMatcher)
		})

		time.Sleep(time.Second)
		require.Equal(t, true, isConnectionInState(v1alpha1.SynchronizationFailed))
		assertSynchronizationStatusError(t)

		// get time of synchronization error
		// and lastSync to -6 second back in time to make reconcile allow to attemt application sync
		connectedConnection, err = compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
		require.NoError(t, err)
		require.NotNil(t, connectedConnection.Status.SynchronizationStatus)
		firstSyncAttempt := connectedConnection.Status.SynchronizationStatus.LastAttempt // moment of the first error

		timeBefore := v1.NewTime(time.Now().Add(time.Second * -6))
		connectedConnection.Status.ConnectionStatus.LastSync = timeBefore
		connectedConnection, err = compassConnectionCRClient.Update(context.Background(), connectedConnection, v1.UpdateOptions{})
		require.NoError(t, err)

		// reset the mock
		configurationClientMock.Calls = nil
		// now let's check when second time we will call to director
		err = waitFor(time.Second, testTimeout, func() bool {
			return mockFunctionCalled(&configurationClientMock.Mock, "FetchConfiguration", requestIDCtxMatcher)
		})

		time.Sleep(time.Second)
		require.Equal(t, true, isConnectionInState(v1alpha1.SynchronizationFailed))
		assertSynchronizationStatusError(t)

		// get time of second synchronization error
		connectedConnection, err = compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
		require.NoError(t, err)
		require.NotNil(t, connectedConnection.Status.SynchronizationStatus)
		secondSyncAttempt := connectedConnection.Status.SynchronizationStatus.LastAttempt

		passedSeconds := secondSyncAttempt.Unix() - firstSyncAttempt.Unix()
		minimalConfigSyncTime.Seconds()
		t.Logf("Number of seconds between attempts: %d", passedSeconds)

		require.Equal(t, true, passedSeconds >= int64(minimalConfigSyncTime/time.Second))

		configurationClientMock.AssertNumberOfCalls(t, "FetchConfiguration", 1)

		// restore previous director mock configuration to not interfere with other tests
		clearMockCalls(&configurationClientMock.Mock)
		configurationClientMock.On("FetchConfiguration", requestIDCtxMatcher).Return(kymaModelApps, graphql.Labels{}, nil)
		configurationClientMock.On("SetURLsLabels", requestIDCtxMatcher, runtimeURLsConfig, graphql.Labels{}).Return(runtimeLabels, nil)
	})

	t.Run("Compass Connection should be in SynchronizationFailed state if failed create Director config client", func(t *testing.T) {
		// given
		assertRecreateSynchronizedConnection(t)
		require.Equal(t, true, isConnectionInState(v1alpha1.Synchronized))
		clientsProviderMock.ExpectedCalls = nil
		clientsProviderMock.Calls = nil
		directorProxyConfiguratorMock.ExpectedCalls = nil
		directorProxyConfiguratorMock.On("SetURLAndCerts", mock.AnythingOfType("cache.ConnectionData")).Return(nil)
		clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(tokensConnectorClientMock, nil)
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(certsConnectorClientMock, nil)
		clientsProviderMock.On("GetDirectorClient", runtimeConfig).Return(nil, errors.New("error"))

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&clientsProviderMock.Mock, "GetDirectorClient", runtimeConfig)
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.SynchronizationFailed))
		assertManagementInfoSetInCR(t)
		assertSynchronizationStatusError(t)
	})

	t.Run("Compass Connection should be in SynchronizationFailed state if failed to read runtime configuration", func(t *testing.T) {
		// given
		configProviderMock.ExpectedCalls = nil
		configProviderMock.Calls = nil
		directorProxyConfiguratorMock.ExpectedCalls = nil
		directorProxyConfiguratorMock.On("SetURLAndCerts", mock.AnythingOfType("cache.ConnectionData")).Return(nil)
		configProviderMock.On("GetConnectionConfig").Return(connectionConfig, nil)
		configProviderMock.On("GetRuntimeConfig").Return(runtimeConfig, errors.New("error"))

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&configProviderMock.Mock, "GetRuntimeConfig")
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.SynchronizationFailed))
		assertManagementInfoSetInCR(t)
		assertSynchronizationStatusError(t)
	})

	t.Run("Compass Connection should be in ConnectionMaintenanceFailed if Management Info is empty and no error", func(t *testing.T) {
		// given
		certsConnectorClientMock.ExpectedCalls = nil
		certsConnectorClientMock.On("Configuration", requestIDCtxMatcher, nilHeaders).Return(gqlschema.Configuration{ManagementPlaneInfo: nil}, nil)

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&certsConnectorClientMock.Mock, "Configuration", requestIDCtxMatcher, nilHeaders)
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.ConnectionMaintenanceFailed))
		assertManagementInfoSetInCR(t)
		assertConnectionStatusSet(t)
	})

	t.Run("Compass Connection should be in ConnectionMaintenanceFailed if failed to access Connector Configuration query", func(t *testing.T) {
		// given
		certsConnectorClientMock.ExpectedCalls = nil
		certsConnectorClientMock.On("Configuration", requestIDCtxMatcher, nilHeaders).Return(gqlschema.Configuration{}, errors.New("error"))

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&certsConnectorClientMock.Mock, "Configuration", requestIDCtxMatcher, nilHeaders)
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.ConnectionMaintenanceFailed))
		assertConnectionStatusSet(t)
	})

	t.Run("Compass Connection should be in ConnectionMaintenanceFailed state if failed create Cert secured client", func(t *testing.T) {
		// given
		clientsProviderMock.ExpectedCalls = nil
		clientsProviderMock.On("GetConnectorCertSecuredClient").Return(nil, errors.New("error"))

		// when
		err = waitFor(checkInterval, testTimeout, func() bool {
			return mockFunctionCalled(&clientsProviderMock.Mock, "GetConnectorCertSecuredClient")
		})

		// then
		require.NoError(t, err)
		require.NoError(t, waitForResourceUpdate(v1alpha1.ConnectionMaintenanceFailed))
		assertConnectionStatusSet(t)
	})
}

func TestFailedToInitializeConnection(t *testing.T) {

	syncPeriodTime := syncPeriod
	ctrlManager, err := manager.New(cfg, manager.Options{SyncPeriod: &syncPeriodTime})
	require.NoError(t, err)

	// Connector token client
	connectorTokenClientMock := connectorTokensClientMock(requestIDCtxMatcher)
	// Config provider
	configProviderMock := configProviderMock()
	// Clients provider
	clientsProviderMock := clientsProviderMock(nil, connectorTokenClientMock, nil)

	// Credentials manager
	credentialsManagerMock := &certsMocks.Manager{}

	var baseDependencies = DependencyConfig{
		K8sConfig:         cfg,
		ControllerManager: ctrlManager,

		ClientsProvider:              clientsProviderMock,
		CredentialsManager:           credentialsManagerMock,
		SynchronizationService:       nil,
		ConfigProvider:               configProviderMock,
		CertValidityRenewalThreshold: 0.3,
		MinimalCompassSyncTime:       minimalConfigSyncTime,
	}

	supervisor, err := baseDependencies.InitializeController()
	require.NoError(t, err)

	defer func() {
		err := compassConnectionCRClient.Delete(context.Background(), compassConnectionName, v1.DeleteOptions{})
		if err != nil {
			t.Logf("error while deleting Compass Connection: %s", err.Error())
		}
	}()
	cancelFunc, _ := StartTestManager(t, ctrlManager)
	defer cancelFunc()

	initConnectionIfNotExist := func() {
		_, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				t.Fatalf("Failed to initialize Connection: %s", err.Error())
			}

			connection, err := supervisor.InitializeCompassConnection(testCtx)
			require.NoError(t, err)
			assert.NotEmpty(t, connection)
		}
	}

	for _, test := range []struct {
		description  string
		setupFunc    func()
		waitFunction func() bool
	}{
		{
			description: "failed to preserve credentials",
			setupFunc: func() {
				credentialsManagerMock.Calls = nil
				credentialsManagerMock.On("PreserveCredentials", mock.AnythingOfType("certificates.Credentials")).Return(errors.New("error"))
			},
			waitFunction: func() bool {
				return mockFunctionCalled(&credentialsManagerMock.Mock, "PreserveCredentials", mock.AnythingOfType("certificates.Credentials"))
			},
		},
		{
			description: "failed to sign CSR",
			setupFunc: func() {
				clearMockCalls(&connectorTokenClientMock.Mock)
				connectorTokenClientMock.On("Configuration", requestIDCtxMatcher, connectorTokenHeadersFunc).Return(connectorConfigurationResponse, nil)
				connectorTokenClientMock.On("SignCSR", requestIDCtxMatcher, mock.AnythingOfType("string"), connectorTokenHeadersFunc).Return(gqlschema.CertificationResult{}, errors.New("error"))
			},
			waitFunction: func() bool {
				return mockFunctionCalled(&connectorTokenClientMock.Mock, "SignCSR", mock.AnythingOfType("string"), connectorTokenHeaders)
			},
		},
		{
			description: "failed to fetch Configuration",
			setupFunc: func() {
				clearMockCalls(&connectorTokenClientMock.Mock)
				connectorTokenClientMock.On("Configuration", requestIDCtxMatcher, connectorTokenHeadersFunc).Return(gqlschema.Configuration{}, errors.New("error"))
				connectorTokenClientMock.On("SignCSR", requestIDCtxMatcher, mock.AnythingOfType("string"), connectorTokenHeadersFunc).Return(gqlschema.CertificationResult{}, errors.New("error"))
			},
			waitFunction: func() bool {
				return mockFunctionCalled(&connectorTokenClientMock.Mock, "Configuration", requestIDCtxMatcher, connectorTokenHeaders)
			},
		},
		{
			description: "failed to get Connector client",
			setupFunc: func() {
				clearMockCalls(&clientsProviderMock.Mock)
				clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(nil, errors.New("error"))
			},
			waitFunction: func() bool {
				return mockFunctionCalled(&connectorTokenClientMock.Mock, "GetConnectorTokensClient", connectorURL)
			},
		},
		{
			description: "connector URL is empty",
			setupFunc: func() {
				clearMockCalls(&configProviderMock.Mock)
				configProviderMock.On("GetConnectionConfig").Return(config.ConnectionConfig{Token: token}, nil)
			},
			waitFunction: func() bool {

				return mockFunctionCalled(&configProviderMock.Mock, "GetConnectionConfig")

			},
		},
		{
			description: "failed to get connection config",
			setupFunc: func() {
				clearMockCalls(&configProviderMock.Mock)
				configProviderMock.On("GetConnectionConfig").Return(config.ConnectionConfig{}, errors.New("error"))
			},
			waitFunction: func() bool {
				fakeT := &testing.T{}
				called := configProviderMock.AssertCalled(fakeT, "GetConnectionConfig")
				return called
			},
		},
	} {
		t.Run("Compass Connection should be in ConnectionFailed state when "+test.description, func(t *testing.T) {
			// given
			test.setupFunc()
			initConnectionIfNotExist()

			// when
			test.waitFunction()

			// then
			require.NoError(t, waitForResourceUpdate(v1alpha1.ConnectionFailed))
		})
	}
}

func waitFor(interval, timeout time.Duration, isDone func() bool) error {
	done := time.After(timeout)

	for {
		if isDone() {
			return nil
		}

		select {
		case <-done:
			return errors.New("timeout waiting for condition")
		default:
			time.Sleep(interval)
		}
	}
}

func clearMockCalls(mock *mock.Mock) {
	mock.ExpectedCalls = nil
	mock.Calls = nil
}

func mockFunctionCalled(mock *mock.Mock, methodName string, arguments ...interface{}) bool {
	fakeT := &testing.T{}
	return mock.AssertCalled(fakeT, methodName, arguments...)
}

func waitForResynchronization() {
	time.Sleep(minimalConfigSyncTime * 2)
}

func waitForResourceUpdate(expectedState v1alpha1.ConnectionState) error {
	// Initial sleep in case if Connection will change from expected state to different
	time.Sleep(2 * time.Second)

	return waitFor(1*time.Second, 4*time.Second, func() bool {
		return isConnectionInState(expectedState)
	})
}

func isConnectionInState(expectedState v1alpha1.ConnectionState) bool {
	connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
	if err != nil {
		return false
	}

	return connectedConnection.Status.State == expectedState
}

func assertConnectionStatusError(t *testing.T) {
	connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
	require.NoError(t, err)
	t.Logf("Connection status error: %s", connectedConnection.Status.ConnectionStatus.Error)
	assert.NotEmpty(t, connectedConnection.Status.ConnectionStatus.Error)
}

func assertSynchronizationStatusError(t *testing.T) {
	connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
	require.NoError(t, err)
	t.Logf("Synchronization status error: %s", connectedConnection.Status.SynchronizationStatus.Error)
	assert.NotEmpty(t, connectedConnection.Status.SynchronizationStatus.Error)
}

func assertManagementInfoSetInCR(t *testing.T) {
	connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, directorURL, connectedConnection.Spec.ManagementInfo.DirectorURL)
	assert.Equal(t, certSecuredConnectorURL, connectedConnection.Spec.ManagementInfo.ConnectorURL)
}

func assertCompassConnectionState(t *testing.T, expectedState v1alpha1.ConnectionState) {
	connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, expectedState, connectedConnection.Status.State)
}

func assertConnectionStatusSet(t *testing.T) {
	connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, connectedConnection.Status.ConnectionStatus)
	assert.NotEmpty(t, connectedConnection.Status.ConnectionStatus)
	assert.NotEmpty(t, connectedConnection.Status.ConnectionStatus.CertificateStatus)
}

func assertRecreateSynchronizedConnection(t *testing.T) {
	err := compassConnectionCRClient.Delete(context.Background(), compassConnectionName, v1.DeleteOptions{})
	require.NoError(t, err)

	// then
	err = waitFor(checkInterval, testTimeout, func() bool {
		return isConnectionInState(v1alpha1.Synchronized)
	})
	require.NoError(t, err)
	assertConnectionStatusSet(t)
	assertManagementInfoSetInCR(t)
}

func assertCertificateRenewed(t *testing.T) {
	connectedConnection, err := compassConnectionCRClient.Get(context.Background(), compassConnectionName, v1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, connectedConnection.Status.ConnectionStatus)
	assert.NotEmpty(t, connectedConnection.Status.ConnectionStatus.CertificateStatus)
	assert.True(t, connectedConnection.Status.ConnectionStatus.Established.Unix() < connectedConnection.Status.ConnectionStatus.Renewed.Unix())
}

func clientsProviderMock(configClient *directorMocks.DirectorClient, connectorTokensClient, connectorCertsClient *connectorMocks.Client) *compassMocks.ClientsProvider {
	clientsProviderMock := &compassMocks.ClientsProvider{}
	clientsProviderMock.On("GetDirectorClient", runtimeConfig).Return(configClient, nil)
	clientsProviderMock.On("GetConnectorCertSecuredClient").Return(connectorCertsClient, nil)
	clientsProviderMock.On("GetConnectorTokensClient", connectorURL).Return(connectorTokensClient, nil)

	return clientsProviderMock
}

func connectorCertClientMock(contextMatcher interface{}) *connectorMocks.Client {
	connectorMock := &connectorMocks.Client{}
	connectorMock.On("Configuration", contextMatcher, nilHeaders).Return(connectorConfigurationResponse, nil)
	connectorMock.On("SignCSR", contextMatcher, mock.AnythingOfType("string"), nilHeaders).Return(connectorCertResponse, nil)

	return connectorMock
}

func connectorTokensClientMock(contextMatcher interface{}) *connectorMocks.Client {
	connectorMock := &connectorMocks.Client{}
	connectorMock.On("Configuration", contextMatcher, connectorTokenHeadersFunc).Return(connectorConfigurationResponse, nil)
	connectorMock.On("SignCSR", contextMatcher, mock.AnythingOfType("string"), connectorTokenHeadersFunc).Return(connectorCertResponse, nil)

	return connectorMock
}

func configProviderMock() *configMocks.Provider {
	providerMock := &configMocks.Provider{}
	providerMock.On("GetConnectionConfig").Return(connectionConfig, nil)
	providerMock.On("GetRuntimeConfig").Return(runtimeConfig, nil)

	return providerMock
}

// StartTestManager
func StartTestManager(t *testing.T, mgr manager.Manager) (context.CancelFunc, *sync.WaitGroup) {
	ctx, cancel := context.WithCancel(context.Background())

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}()
	return cancel, wg
}
