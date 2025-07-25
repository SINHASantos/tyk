package gateway

import (
	"crypto/tls"
	"fmt"
	"github.com/TykTechnologies/tyk/common/option"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	texttemplate "text/template"

	"github.com/gorilla/mux"
	"github.com/justinas/alice"
	"github.com/sirupsen/logrus"

	"github.com/TykTechnologies/tyk/apidef"
	"github.com/TykTechnologies/tyk/coprocess"
	"github.com/TykTechnologies/tyk/rpc"
	"github.com/TykTechnologies/tyk/storage"
	"github.com/TykTechnologies/tyk/trace"

	"github.com/TykTechnologies/tyk/internal/httpctx"
	"github.com/TykTechnologies/tyk/internal/httputil"
	"github.com/TykTechnologies/tyk/internal/otel"
	"github.com/TykTechnologies/tyk/internal/service/newrelic"
)

const (
	rateLimitEndpoint = "/tyk/rate-limits/"
)

type ChainObject struct {
	ThisHandler    http.Handler
	RateLimitChain http.Handler
	Open           bool
	Skip           bool
}

// ProcessSpecOptions represents options for processSpec method
type ProcessSpecOptions struct {
	quotaKey string
}

func (gw *Gateway) prepareStorage() generalStores {
	var gs generalStores

	gs.redisStore = &storage.RedisCluster{KeyPrefix: "apikey-", HashKeys: gw.GetConfig().HashKeys, ConnectionHandler: gw.StorageConnectionHandler}
	gs.redisStore.Connect()

	gs.redisOrgStore = &storage.RedisCluster{KeyPrefix: "orgkey.", ConnectionHandler: gw.StorageConnectionHandler}
	gs.redisOrgStore.Connect()

	gs.healthStore = &storage.RedisCluster{KeyPrefix: "apihealth.", ConnectionHandler: gw.StorageConnectionHandler}
	gs.healthStore.Connect()

	gs.rpcAuthStore = &RPCStorageHandler{KeyPrefix: "apikey-", HashKeys: gw.GetConfig().HashKeys, Gw: gw}
	gs.rpcOrgStore = gw.getGlobalMDCBStorageHandler("orgkey.", false)

	gw.GlobalSessionManager.Init(gs.redisStore)
	return gs
}

func (gw *Gateway) skipSpecBecauseInvalid(spec *APISpec, logger *logrus.Entry) bool {
	switch spec.Protocol {
	case "", "http", "https":
		if spec.Proxy.ListenPath == "" {
			logger.Error("Listen path is empty")
			return true
		}
		if strings.Contains(spec.Proxy.ListenPath, " ") {
			logger.Error("Listen path contains spaces, is invalid")
			return true
		}
	}
	if val, err := gw.kvStore(spec.Proxy.TargetURL); err == nil {
		spec.Proxy.TargetURL = val
	}

	_, err := url.Parse(spec.Proxy.TargetURL)
	if err != nil {
		logger.Error("couldn't parse target URL: ", err)
		return true
	}

	return false
}

func generateDomainPath(hostname, listenPath string) string {
	return hostname + listenPath
}

func countApisByListenHash(specs []*APISpec) map[string]int {
	count := make(map[string]int, len(specs))
	// We must track the hostname no matter what
	for _, spec := range specs {
		domain := spec.GetAPIDomain()
		domainHash := generateDomainPath(domain, spec.Proxy.ListenPath)
		if count[domainHash] == 0 {
			if domain == "" {
				domain = "(no host)"
			}
			mainLog.WithFields(logrus.Fields{
				"api_name": spec.Name,
				"domain":   domain,
			}).Info("Tracking hostname")
		}
		count[domainHash]++
	}
	return count
}

func fixFuncPath(pathPrefix string, funcs []apidef.MiddlewareDefinition) {
	for index := range funcs {
		funcs[index].Path = filepath.Join(pathPrefix, funcs[index].Path)
	}
}

func (gw *Gateway) generateSubRoutes(spec *APISpec, router *mux.Router) {
	if spec.GraphQL.GraphQLPlayground.Enabled {
		gw.loadGraphQLPlayground(spec, router)
	}

	if spec.EnableBatchRequestSupport {
		gw.addBatchEndpoint(spec, router)
	}

	if spec.UseOauth2 {
		oauthManager := gw.addOAuthHandlers(spec, router)
		spec.OAuthManager = oauthManager
	}
}

func (gw *Gateway) processSpec(
	spec *APISpec,
	apisByListen map[string]int,
	gs *generalStores,
	logger *logrus.Entry,
	opts ...option.Option[ProcessSpecOptions],
) *ChainObject {

	var options = option.New(opts).Build(ProcessSpecOptions{
		quotaKey: "",
	})

	var chainDef ChainObject

	logger = logger.WithFields(logrus.Fields{
		"org_id":   spec.OrgID,
		"api_id":   spec.APIID,
		"api_name": spec.Name,
		"type":     traceLogRequest.String(),
	})

	var coprocessLog = logger.WithFields(logrus.Fields{
		"prefix": "coprocess",
	})

	if spec.Proxy.Transport.SSLMaxVersion > 0 {
		spec.Proxy.Transport.SSLMaxVersion = tls.VersionTLS12
	}

	if spec.Proxy.Transport.SSLMinVersion > spec.Proxy.Transport.SSLMaxVersion {
		spec.Proxy.Transport.SSLMaxVersion = spec.Proxy.Transport.SSLMinVersion
	}

	if len(spec.TagHeaders) > 0 {
		// Ensure all headers marked for tagging are lowercase
		lowerCaseHeaders := make([]string, len(spec.TagHeaders))
		for i, k := range spec.TagHeaders {
			lowerCaseHeaders[i] = strings.ToLower(k)

		}
		spec.TagHeaders = lowerCaseHeaders
	}

	if gw.skipSpecBecauseInvalid(spec, logger) {
		logger.Warning("Spec not valid, skipped!")
		chainDef.Skip = true
		return &chainDef
	}

	// Expose API only to looping
	if spec.Internal {
		chainDef.Skip = true
	}

	pathModified := false
	for {
		domain := spec.GetAPIDomain()
		hash := generateDomainPath(domain, spec.Proxy.ListenPath)

		if apisByListen[hash] < 2 {
			// not a duplicate
			break
		}
		if !pathModified {
			prev := gw.getApiSpec(spec.APIID)
			if prev != nil && prev.Proxy.ListenPath == spec.Proxy.ListenPath {
				// if this APIID was already loaded and
				// had this listen path, let it keep it.
				break
			}
			spec.Proxy.ListenPath += "-" + spec.APIID
			pathModified = true
		} else {
			// keep adding '_' chars
			spec.Proxy.ListenPath += "_"
		}
	}
	if pathModified {
		logger.Error("Listen path collision, changed to ", spec.Proxy.ListenPath)
	}

	// Set up LB targets:
	if spec.Proxy.EnableLoadBalancing {
		sl := apidef.NewHostListFromList(spec.Proxy.Targets)
		spec.Proxy.StructuredTargetList = sl
	}

	// Initialise the auth and session managers (use Redis for now)
	authStore, orgStore, sessionStore := gw.configureAuthAndOrgStores(gs, spec)

	// Health checkers are initialised per spec so that each API handler has it's own connection and redis storage pool
	spec.Init(authStore, sessionStore, gs.healthStore, orgStore)

	// Set up all the JSVM middleware
	var mwAuthCheckFunc apidef.MiddlewareDefinition
	mwPreFuncs := []apidef.MiddlewareDefinition{}
	mwPostFuncs := []apidef.MiddlewareDefinition{}
	mwPostAuthCheckFuncs := []apidef.MiddlewareDefinition{}
	mwResponseFuncs := []apidef.MiddlewareDefinition{}

	var mwDriver apidef.MiddlewareDriver

	var prefix string
	if !spec.CustomMiddlewareBundleDisabled && spec.CustomMiddlewareBundle != "" {
		prefix = gw.getBundleDestPath(spec)
	}

	logger.Debug("Initializing API")
	var mwPaths []string

	mwPaths, mwAuthCheckFunc, mwPreFuncs, mwPostFuncs, mwPostAuthCheckFuncs, mwResponseFuncs, mwDriver = gw.loadCustomMiddleware(spec)
	if gw.GetConfig().EnableJSVM && (spec.hasVirtualEndpoint() || mwDriver == apidef.OttoDriver) {
		logger.Debug("Loading JS Paths")
		spec.JSVM.LoadJSPaths(mwPaths, prefix)
	}

	//  if bundle was used - fix paths for goplugin-type custom middle-wares
	if mwDriver == apidef.GoPluginDriver && prefix != "" {
		mwAuthCheckFunc.Path = filepath.Join(prefix, mwAuthCheckFunc.Path)
		fixFuncPath(prefix, mwPreFuncs)
		fixFuncPath(prefix, mwPostFuncs)
		fixFuncPath(prefix, mwPostAuthCheckFuncs)
		fixFuncPath(prefix, mwResponseFuncs)
	}

	enableVersionOverrides := false
	for _, versionData := range spec.VersionData.Versions {
		if versionData.OverrideTarget != "" && !spec.VersionData.NotVersioned {
			enableVersionOverrides = true
			break
		}
	}

	// Already vetted
	spec.target, _ = url.Parse(spec.Proxy.TargetURL)

	var proxy ReturningHttpHandler
	if enableVersionOverrides {
		logger.Info("Multi target enabled")
		proxy = gw.NewMultiTargetProxy(spec, logger)
	} else {
		proxy = gw.TykNewSingleHostReverseProxy(spec.target, spec, logger)
	}

	// Create the response processors, pass all the loaded custom middleware response functions:
	spec.ResponseChain = gw.createResponseMiddlewareChain(spec, mwResponseFuncs, logger)

	baseMid := NewBaseMiddleware(gw, spec, proxy, logger)

	keyPrefix := "cache-" + spec.APIID
	cacheStore := storage.RedisCluster{KeyPrefix: keyPrefix, IsCache: true, ConnectionHandler: gw.StorageConnectionHandler}
	cacheStore.Connect()

	var chain http.Handler
	var chainArray []alice.Constructor
	var authArray []alice.Constructor

	if spec.UseKeylessAccess {
		chainDef.Open = true
		logger.Info("Checking security policy: Open")
	}

	gw.mwAppendEnabled(&chainArray, &VersionCheck{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &CORSMiddleware{BaseMiddleware: baseMid.Copy()})

	for _, obj := range mwPreFuncs {
		if mwDriver == apidef.GoPluginDriver {
			gw.mwAppendEnabled(
				&chainArray,
				&GoPluginMiddleware{
					BaseMiddleware: baseMid.Copy(),
					Path:           obj.Path,
					SymbolName:     obj.Name,
					APILevel:       true,
				},
			)
		} else if mwDriver != apidef.OttoDriver {
			coprocessLog.Debug("Registering coprocess middleware, hook name: ", obj.Name, "hook type: Pre", ", driver: ", mwDriver)
			gw.mwAppendEnabled(&chainArray, &CoProcessMiddleware{baseMid.Copy(), coprocess.HookType_Pre, obj.Name, mwDriver, obj.RawBodyOnly, nil})
		} else {
			chainArray = append(chainArray, gw.createDynamicMiddleware(obj.Name, true, obj.RequireSession, baseMid.Copy()))
		}
	}

	gw.mwAppendEnabled(&chainArray, &RateCheckMW{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &IPWhiteListMiddleware{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &IPBlackListMiddleware{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &CertificateCheckMW{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &OrganizationMonitor{BaseMiddleware: baseMid.Copy(), mon: Monitor{Gw: gw}})
	gw.mwAppendEnabled(&chainArray, &RequestSizeLimitMiddleware{baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &MiddlewareContextVars{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &TrackEndpointMiddleware{baseMid.Copy()})

	if !spec.UseKeylessAccess {
		// Select the keying method to use for setting session states
		if gw.mwAppendEnabled(&authArray, &Oauth2KeyExists{baseMid.Copy()}) {
			logger.Info("Checking security policy: OAuth")
		}

		if gw.mwAppendEnabled(&authArray, &ExternalOAuthMiddleware{baseMid.Copy()}) {
			logger.Info("Checking security policy: External OAuth")
		}

		if gw.mwAppendEnabled(&authArray, &BasicAuthKeyIsValid{baseMid.Copy(), nil, nil}) {
			logger.Info("Checking security policy: Basic")
		}

		if gw.mwAppendEnabled(&authArray, &HTTPSignatureValidationMiddleware{BaseMiddleware: baseMid.Copy()}) {
			logger.Info("Checking security policy: HMAC")
		}

		if gw.mwAppendEnabled(&authArray, &JWTMiddleware{baseMid.Copy()}) {
			logger.Info("Checking security policy: JWT")
		}

		if gw.mwAppendEnabled(&authArray, &OpenIDMW{BaseMiddleware: baseMid.Copy()}) {
			logger.Info("Checking security policy: OpenID")
		}

		customPluginAuthEnabled := spec.CustomPluginAuthEnabled || spec.UseGoPluginAuth || spec.EnableCoProcessAuth

		if customPluginAuthEnabled && !mwAuthCheckFunc.Disabled {
			switch spec.CustomMiddleware.Driver {
			case apidef.OttoDriver:
				logger.Info("----> Checking security policy: JS Plugin")
				authArray = append(authArray, gw.createMiddleware(&DynamicMiddleware{
					BaseMiddleware:      baseMid.Copy(),
					MiddlewareClassName: mwAuthCheckFunc.Name,
					Pre:                 true,
					Auth:                true,
				}))
			case apidef.GoPluginDriver:
				gw.mwAppendEnabled(
					&authArray,
					&GoPluginMiddleware{
						BaseMiddleware: baseMid.Copy(),
						Path:           mwAuthCheckFunc.Path,
						SymbolName:     mwAuthCheckFunc.Name,
						APILevel:       true,
					},
				)
			default:
				coprocessLog.Debug("Registering coprocess middleware, hook name: ", mwAuthCheckFunc.Name, "hook type: CustomKeyCheck", ", driver: ", mwDriver)

				newExtractor(spec, baseMid.Copy())
				gw.mwAppendEnabled(&authArray, &CoProcessMiddleware{baseMid.Copy(), coprocess.HookType_CustomKeyCheck, mwAuthCheckFunc.Name, mwDriver, mwAuthCheckFunc.RawBodyOnly, nil})
			}
		}

		if spec.UseStandardAuth || len(authArray) == 0 {
			logger.Info("Checking security policy: Token")
			authArray = append(authArray, gw.createMiddleware(&AuthKey{baseMid.Copy()}))
		}

		chainArray = append(chainArray, authArray...)

		// if gw is edge, then prefetch any existent org session expiry
		if gw.GetConfig().SlaveOptions.UseRPC {
			// if not in emergency so load from backup is not blocked
			if !rpc.IsEmergencyMode() {
				baseMid.OrgSessionExpiry(spec.OrgID)
			}
		}

		for _, obj := range mwPostAuthCheckFuncs {
			if mwDriver == apidef.GoPluginDriver {
				gw.mwAppendEnabled(
					&chainArray,
					&GoPluginMiddleware{
						BaseMiddleware: baseMid.Copy(),
						Path:           obj.Path,
						SymbolName:     obj.Name,
						APILevel:       true,
					},
				)
			} else {
				coprocessLog.Debug("Registering coprocess middleware, hook name: ", obj.Name, "hook type: Pre", ", driver: ", mwDriver)
				gw.mwAppendEnabled(&chainArray, &CoProcessMiddleware{baseMid.Copy(), coprocess.HookType_PostKeyAuth, obj.Name, mwDriver, obj.RawBodyOnly, nil})
			}
		}

		gw.mwAppendEnabled(&chainArray, &StripAuth{baseMid.Copy()})
		gw.mwAppendEnabled(&chainArray, &KeyExpired{baseMid.Copy()})
		gw.mwAppendEnabled(&chainArray, &AccessRightsCheck{baseMid.Copy()})
		gw.mwAppendEnabled(&chainArray, &GranularAccessMiddleware{baseMid.Copy()})
		gw.mwAppendEnabled(&chainArray, &RateLimitAndQuotaCheck{baseMid.Copy()})
	}

	gw.mwAppendEnabled(&chainArray, &RateLimitForAPI{BaseMiddleware: baseMid.Copy(), quotaKey: options.quotaKey})
	gw.mwAppendEnabled(&chainArray, &GraphQLMiddleware{BaseMiddleware: baseMid.Copy()})

	if streamMw := getStreamingMiddleware(baseMid); streamMw != nil {
		gw.mwAppendEnabled(&chainArray, streamMw)
	}

	if !spec.UseKeylessAccess {
		gw.mwAppendEnabled(&chainArray, &GraphQLComplexityMiddleware{BaseMiddleware: baseMid.Copy()})
		gw.mwAppendEnabled(&chainArray, &GraphQLGranularAccessMiddleware{BaseMiddleware: baseMid.Copy()})
	}

	if upstreamBasicAuthMw := getUpstreamBasicAuthMw(baseMid); upstreamBasicAuthMw != nil {
		gw.mwAppendEnabled(&chainArray, upstreamBasicAuthMw)
	}

	if upstreamOAuthMw := getUpstreamOAuthMw(baseMid); upstreamOAuthMw != nil {
		gw.mwAppendEnabled(&chainArray, upstreamOAuthMw)
	}

	gw.mwAppendEnabled(&chainArray, &ValidateJSON{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &ValidateRequest{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &PersistGraphQLOperationMiddleware{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &TransformMiddleware{baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &TransformJQMiddleware{baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &TransformHeaders{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &URLRewriteMiddleware{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &TransformMethod{BaseMiddleware: baseMid.Copy()})

	// Earliest we can respond with cache get 200 ok
	gw.mwAppendEnabled(&chainArray, newMockResponseMiddleware(baseMid.Copy()))
	gw.mwAppendEnabled(&chainArray, &RedisCacheMiddleware{BaseMiddleware: baseMid.Copy(), store: &cacheStore})
	gw.mwAppendEnabled(&chainArray, &VirtualEndpoint{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &RequestSigning{BaseMiddleware: baseMid.Copy()})
	gw.mwAppendEnabled(&chainArray, &GoPluginMiddleware{BaseMiddleware: baseMid.Copy()})

	for _, obj := range mwPostFuncs {
		if mwDriver == apidef.GoPluginDriver {
			gw.mwAppendEnabled(
				&chainArray,
				&GoPluginMiddleware{
					BaseMiddleware: baseMid.Copy(),
					Path:           obj.Path,
					SymbolName:     obj.Name,
					APILevel:       true,
				},
			)
		} else if mwDriver != apidef.OttoDriver {
			coprocessLog.Debug("Registering coprocess middleware, hook name: ", obj.Name, "hook type: Post", ", driver: ", mwDriver)
			gw.mwAppendEnabled(&chainArray, &CoProcessMiddleware{baseMid.Copy(), coprocess.HookType_Post, obj.Name, mwDriver, obj.RawBodyOnly, nil})
		} else {
			chainArray = append(chainArray, gw.createDynamicMiddleware(obj.Name, false, obj.RequireSession, baseMid.Copy()))
		}
	}
	chain = alice.New(chainArray...).Then(&DummyProxyHandler{SH: SuccessHandler{baseMid.Copy()}, Gw: gw})

	if !spec.UseKeylessAccess {
		var simpleArray []alice.Constructor
		gw.mwAppendEnabled(&simpleArray, &IPWhiteListMiddleware{baseMid.Copy()})
		gw.mwAppendEnabled(&simpleArray, &IPBlackListMiddleware{BaseMiddleware: baseMid.Copy()})
		gw.mwAppendEnabled(&simpleArray, &OrganizationMonitor{BaseMiddleware: baseMid.Copy(), mon: Monitor{Gw: gw}})
		gw.mwAppendEnabled(&simpleArray, &VersionCheck{BaseMiddleware: baseMid.Copy()})
		simpleArray = append(simpleArray, authArray...)
		gw.mwAppendEnabled(&simpleArray, &KeyExpired{baseMid.Copy()})
		gw.mwAppendEnabled(&simpleArray, &AccessRightsCheck{baseMid.Copy()})

		rateLimitPath := path.Join(spec.Proxy.ListenPath, rateLimitEndpoint)
		logger.Debug("Rate limit endpoint is: ", rateLimitPath)

		chainDef.RateLimitChain = alice.New(simpleArray...).
			Then(http.HandlerFunc(userRatesCheck))
	}

	logger.Debug("Setting Listen Path: ", spec.Proxy.ListenPath)

	if trace.IsEnabled() { // trace.IsEnabled = check if opentracing is enabled
		chainDef.ThisHandler = trace.Handle(spec.Name, chain)
	} else if gw.GetConfig().OpenTelemetry.Enabled { // check if opentelemetry is enabled
		spanAttrs := []otel.SpanAttribute{}
		spanAttrs = append(spanAttrs, otel.ApidefSpanAttributes(spec.APIDefinition)...)
		chainDef.ThisHandler = otel.HTTPHandler(spec.Name, chain, gw.TracerProvider, spanAttrs...)
	} else {
		chainDef.ThisHandler = chain
	}

	if spec.APIDefinition.AnalyticsPlugin.Enabled {

		ap := &GoAnalyticsPlugin{
			Path:     spec.AnalyticsPlugin.PluginPath,
			FuncName: spec.AnalyticsPlugin.FuncName,
		}

		if ap.loadAnalyticsPlugin() {
			spec.AnalyticsPluginConfig = ap
			logger.Debug("Loaded analytics plugin")
		}
	}

	logger.WithFields(logrus.Fields{
		"prefix":      "gateway",
		"user_ip":     "--",
		"server_name": "--",
		"user_id":     "--",
	}).Info("API Loaded")

	return &chainDef
}

func (gw *Gateway) configureAuthAndOrgStores(gs *generalStores, spec *APISpec) (storage.Handler, storage.Handler, storage.Handler) {
	authStore := gs.redisStore
	orgStore := gs.redisOrgStore

	switch spec.AuthProvider.StorageEngine {
	case LDAPStorageEngine:
		storageEngine := LDAPStorageHandler{}
		storageEngine.LoadConfFromMeta(spec.AuthProvider.Meta)
		authStore = &storageEngine
	case RPCStorageEngine:
		authStore = gs.rpcAuthStore
		orgStore = gs.rpcOrgStore
		spec.GlobalConfig.EnforceOrgDataAge = true
		globalConf := gw.GetConfig()
		globalConf.EnforceOrgDataAge = true
		gw.SetConfig(globalConf)
	}

	sessionStore := gs.redisStore
	switch spec.SessionProvider.StorageEngine {
	case RPCStorageEngine:
		sessionStore = gs.rpcAuthStore
	}

	return authStore, orgStore, sessionStore
}

// Check for recursion
const defaultLoopLevelLimit = 5

func isLoop(r *http.Request) (bool, error) {
	if r.URL.Scheme != "tyk" {
		return false, nil
	}

	limit := ctxLoopLevelLimit(r)
	if limit == 0 {
		limit = defaultLoopLevelLimit
	}

	if ctxLoopLevel(r) > limit {
		return true, fmt.Errorf("Loop level too deep. Found more than %d loops in single request", limit)
	}

	return true, nil
}

type DummyProxyHandler struct {
	SH SuccessHandler
	Gw *Gateway `json:"-"`
}

func (d *DummyProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if newURL := ctxGetURLRewriteTarget(r); newURL != nil {
		r.URL = newURL
		ctxSetURLRewriteTarget(r, nil)
	}
	if newMethod := ctxGetTransformRequestMethod(r); newMethod != "" {
		r.Method = newMethod
		ctxSetTransformRequestMethod(r, "")
	}
	if found, err := isLoop(r); found {
		if err != nil {
			handler := ErrorHandler{d.SH.Base()}
			handler.HandleError(w, r, err.Error(), http.StatusInternalServerError, true)
			return
		}

		r.URL.Scheme = "http"
		if methodOverride := r.URL.Query().Get("method"); methodOverride != "" {
			r.Method = methodOverride
		}

		var handler http.Handler
		if r.URL.Hostname() == "self" {
			httpctx.SetSelfLooping(r, true)
			if h, found := d.Gw.apisHandlesByID.Load(d.SH.Spec.APIID); found {
				if chain, ok := h.(*ChainObject); ok {
					handler = chain.ThisHandler
				} else {
					log.WithFields(logrus.Fields{"api_id": d.SH.Spec.APIID}).Debug("failed to cast stored api handles to *ChainObject")
				}
			}
		} else {
			ctxSetVersionInfo(r, nil)

			if targetAPI := d.Gw.fuzzyFindAPI(r.URL.Hostname()); targetAPI != nil {
				if h, found := d.Gw.apisHandlesByID.Load(targetAPI.APIID); found {
					if chain, ok := h.(*ChainObject); ok {
						handler = chain.ThisHandler
					} else {
						log.WithFields(logrus.Fields{"api_id": d.SH.Spec.APIID}).Debug("failed to cast stored api handles to *ChainObject")
					}
				}
			} else {
				handler := ErrorHandler{d.SH.Base()}
				handler.HandleError(w, r, "Can't detect loop target", http.StatusInternalServerError, true)
				return
			}
		}

		// No need to handle errors, in all error cases limit will be set to 0
		loopLevelLimit, _ := strconv.Atoi(r.URL.Query().Get("loop_limit"))
		ctxSetCheckLoopLimits(r, r.URL.Query().Get("check_limits") == "true")

		if origURL := ctxGetOrigRequestURL(r); origURL != nil {
			r.URL.Host = origURL.Host
			r.URL.RawQuery = origURL.RawQuery
			ctxSetOrigRequestURL(r, nil)
		}

		ctxIncLoopLevel(r, loopLevelLimit)
		handler.ServeHTTP(w, r)
		return
	}

	if d.SH.Spec.target.Scheme == "tyk" {
		handler, _, found := d.Gw.findInternalHttpHandlerByNameOrID(d.SH.Spec.target.Host)

		if !found {
			handler := ErrorHandler{d.SH.Base()}
			handler.HandleError(w, r, "Couldn't detect target", http.StatusInternalServerError, true)
			return
		}

		targetUrl, err := d.SH.Spec.getRedirectTargetUrl(ctxGetInternalRedirectTarget(r))

		if err != nil {
			log.Errorf("failed to create internal redirect url: %s", err)
			handler := ErrorHandler{d.SH.Base()}
			handler.HandleError(w, r, "Failed to perform internal redirect", http.StatusInternalServerError, true)
			return
		}

		d.SH.Spec.SanitizeProxyPaths(r)
		ctxSetInternalRedirectTarget(r, targetUrl)
		ctxSetVersionInfo(r, nil)
		handler.ServeHTTP(w, r)
		return
	}

	d.SH.ServeHTTP(w, r)
}

func (gw *Gateway) findInternalHttpHandlerByNameOrID(apiNameOrID string) (handler http.Handler, targetAPI *APISpec, ok bool) {
	targetAPI = gw.fuzzyFindAPI(apiNameOrID)
	if targetAPI == nil {
		return
	}

	h, found := gw.apisHandlesByID.Load(targetAPI.APIID)
	if !found {
		return nil, nil, false
	}

	return h.(*ChainObject).ThisHandler, targetAPI, true
}

func (gw *Gateway) loadGlobalApps() {
	// we need to make a full copy of the slice, as loadApps will
	// use in-place to sort the apis.
	gw.apisMu.RLock()
	specs := make([]*APISpec, len(gw.apiSpecs))
	copy(specs, gw.apiSpecs)
	gw.apisMu.RUnlock()
	gw.loadApps(specs)
}

func trimCategories(name string) string {
	if i := strings.Index(name, "#"); i != -1 {
		return name[:i-1]
	}

	return name
}

func APILoopingName(name string) string {
	return replaceNonAlphaNumeric(trimCategories(name))
}

func (gw *Gateway) fuzzyFindAPI(search string) *APISpec {
	if search == "" {
		return nil
	}

	gw.apisMu.RLock()
	defer gw.apisMu.RUnlock()

	for _, api := range gw.apisByID {
		if api.APIID == search ||
			api.Id.Hex() == search ||
			strings.EqualFold(APILoopingName(api.Name), search) {

			return api
		}
	}

	return nil
}

type explicitRouteHandler struct {
	prefix  string
	handler http.Handler
}

func (h *explicitRouteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == h.prefix || strings.HasPrefix(r.URL.Path, h.prefix+"/") {
		h.handler.ServeHTTP(w, r)
		return
	}

	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprint(w, http.StatusText(http.StatusNotFound))
}

func explicitRouteSubpaths(prefix string, handler http.Handler, enabled bool) http.Handler {
	// feature is enabled via config option
	if !enabled {
		return handler
	}

	// keep trailing slash paths as-is
	if strings.HasSuffix(prefix, "/") {
		return handler
	}
	// keep paths with params as-is
	if strings.Contains(prefix, "{") && strings.Contains(prefix, "}") {
		return handler
	}

	return &explicitRouteHandler{
		prefix:  prefix,
		handler: handler,
	}
}

// loadHTTPService has two responsibilities:
//
// - register gorilla/mux routing handless with proxyMux directly (wrapped),
// - return a raw http.Handler for tyk://ID urls.
func (gw *Gateway) loadHTTPService(spec *APISpec, apisByListen map[string]int, gs *generalStores, muxer *proxyMux) (*ChainObject, error) {
	// MakeSpec validates listenpath, but we can't be sure that it's in all the invocation paths.
	// Since the check is relatively inexpensive, do it here to prevent issues in uncovered paths.
	if err := httputil.ValidatePath(spec.Proxy.ListenPath); err != nil {
		return nil, fmt.Errorf("invalid listen path while loading api: %w", err)
	}

	gwConfig := gw.GetConfig()
	port := gwConfig.ListenPort
	if spec.ListenPort != 0 {
		port = spec.ListenPort
	}
	router := muxer.router(port, spec.Protocol, gwConfig)
	if router == nil {
		router = mux.NewRouter()
		newrelic.Mount(router, gw.NewRelicApplication)

		muxer.setRouter(port, spec.Protocol, router, gwConfig)
	}

	hostname := gwConfig.HostName
	if gwConfig.EnableCustomDomains && spec.Domain != "" {
		hostname = spec.GetAPIDomain()
	}

	if hostname != "" {
		mainLog.Info("API hostname set: ", hostname)
		router = router.Host(hostname).Subrouter()
	}

	var chainObj *ChainObject
	if curSpec := gw.getApiSpec(spec.APIID); !shouldReloadSpec(curSpec, spec) {
		if chain, found := gw.apisHandlesByID.Load(spec.APIID); found {
			chainObj = chain.(*ChainObject)
		}
	} else {
		chainObj = gw.processSpec(spec, apisByListen, gs, logrus.NewEntry(log))
	}

	if chainObj.Skip {
		return chainObj, nil
	}

	// Prefixes are multiple paths that the API endpoints are listening on.
	prefixes := []string{
		// API definition UUID
		"/" + spec.APIID + "/",
		// User defined listen path
		spec.Proxy.ListenPath,
	}

	// Register routes for each prefix
	for _, prefix := range prefixes {
		subrouter := router.PathPrefix(prefix).Subrouter()

		gw.generateSubRoutes(spec, subrouter)

		if !chainObj.Open {
			subrouter.Handle(rateLimitEndpoint, chainObj.RateLimitChain)
		}

		httpHandler := explicitRouteSubpaths(prefix, chainObj.ThisHandler, gwConfig.HttpServerOptions.EnableStrictRoutes)

		// Attach handlers
		subrouter.NewRoute().Handler(httpHandler)
	}

	return chainObj, nil
}

func (gw *Gateway) loadTCPService(spec *APISpec, gs *generalStores, muxer *proxyMux) {
	// Initialise the auth and session managers (use Redis for now)
	authStore := gs.redisStore
	orgStore := gs.redisOrgStore
	switch spec.AuthProvider.StorageEngine {
	case LDAPStorageEngine:
		storageEngine := LDAPStorageHandler{}
		storageEngine.LoadConfFromMeta(spec.AuthProvider.Meta)
		authStore = &storageEngine
	case RPCStorageEngine:
		authStore = gs.rpcAuthStore
		orgStore = gs.rpcOrgStore
		spec.GlobalConfig.EnforceOrgDataAge = true
		gwConfig := gw.GetConfig()
		gwConfig.EnforceOrgDataAge = true
		gw.SetConfig(gwConfig)
	}

	sessionStore := gs.redisStore
	switch spec.SessionProvider.StorageEngine {
	case RPCStorageEngine:
		sessionStore = gs.rpcAuthStore
	}

	// Health checkers are initialised per spec so that each API handler has it's own connection and redis storage pool
	spec.Init(authStore, sessionStore, gs.healthStore, orgStore)

	muxer.addTCPService(spec, nil, gw)
}

type generalStores struct {
	redisStore, redisOrgStore, healthStore, rpcAuthStore, rpcOrgStore storage.Handler
}

var playgroundTemplate *texttemplate.Template

func (gw *Gateway) readGraphqlPlaygroundTemplate() {
	playgroundPath := filepath.Join(gw.GetConfig().TemplatePath, "playground")
	files, err := ioutil.ReadDir(playgroundPath)
	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "playground",
		}).Error("Could not load the default playground templates: ", err)
	}

	var paths []string
	for _, file := range files {
		paths = append(paths, filepath.Join(playgroundPath, file.Name()))
	}

	playgroundTemplate, err = texttemplate.ParseFiles(paths...)
	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "playground",
		}).Error("Could not parse the default playground templates: ", err)
	}
}

const (
	playgroundJSTemplateName   = "playground.js"
	playgroundHTMLTemplateName = "index.html"
)

func (gw *Gateway) loadGraphQLPlayground(spec *APISpec, subrouter *mux.Router) {
	// endpoint is a graphql server url to which a playground makes the request.

	endpoint := spec.Proxy.ListenPath
	playgroundPath := path.Join("/", spec.GraphQL.GraphQLPlayground.Path)

	// If tyk-cloud is enabled, listen path will be api id and slug is mapped to listen path in nginx config.
	// So, requests should be sent to slug endpoint, nginx will route them to internal gateway's listen path.
	if gw.GetConfig().Cloud {
		endpoint = fmt.Sprintf("/%s/", spec.Slug)
	}

	subrouter.Methods(http.MethodGet).Path(path.Join(playgroundPath, playgroundJSTemplateName)).HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if playgroundTemplate == nil {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err := playgroundTemplate.ExecuteTemplate(rw, playgroundJSTemplateName, nil); err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
		}
	})

	subrouter.Methods(http.MethodGet).Path(playgroundPath).HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if playgroundTemplate == nil {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		err := playgroundTemplate.ExecuteTemplate(rw, playgroundHTMLTemplateName, struct {
			Url, PathPrefix string
		}{endpoint, path.Join(endpoint, playgroundPath)})

		if err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
		}
	})
}

func sortSpecsByListenPath(specs []*APISpec) {
	// sort by listen path from longer to shorter, so that /foo
	// doesn't break /foo-bar
	sort.Slice(specs, func(i, j int) bool {
		// we sort by the following rules:
		// - decreasing order of listen path length
		// - if a domain is empty it should be at the end
		if (specs[i].Domain == "") != (specs[j].Domain == "") {
			return specs[i].Domain != ""
		}

		return listenPathLength(specs[i].Proxy.ListenPath) > listenPathLength(specs[j].Proxy.ListenPath)
	})
}

func listenPathLength(listenPath string) int {
	// If the path doesn't contain '{', compute the length directly
	if !strings.Contains(listenPath, "{") {
		return len(listenPath)
	}

	// Split the path into segments and calculate the total length
	length := strings.Count(listenPath, "/")

	for _, segment := range strings.Split(listenPath, "/") {
		// Skip segments enclosed by {} with non-empty content
		if len(segment) > 2 && segment[0] == '{' && segment[len(segment)-1] == '}' {
			continue
		}
		length += len(segment)
	}

	return length
}

// Create the individual API (app) specs based on live configurations and assign middleware
func (gw *Gateway) loadApps(specs []*APISpec) {
	mainLog.Info("Loading API configurations.")

	tmpSpecRegister := make(map[string]*APISpec)
	tmpSpecHandles := new(sync.Map)

	sortSpecsByListenPath(specs)

	// Create a new handler for each API spec
	apisByListen := countApisByListenHash(specs)

	gwConf := gw.GetConfig()
	port := gwConf.ListenPort

	if gwConf.ControlAPIPort != 0 {
		port = gwConf.ControlAPIPort
	}

	muxer := &proxyMux{
		track404Logs: gwConf.Track404Logs,
	}
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(muxer.handle404)
	gw.loadControlAPIEndpoints(router)

	muxer.setRouter(port, "", router, gw.GetConfig())
	gs := gw.prepareStorage()
	shouldTrace := trace.IsEnabled()

	for _, spec := range specs {
		func() {
			defer func() {
				// recover from panic if one occurred. Set err to nil otherwise.
				if err := recover(); err != nil {
					if err := recoverFromLoadApiPanic(spec, err); err != nil {
						log.Error(err)
					}
				}
			}()

			if spec.ListenPort != spec.GlobalConfig.ListenPort {
				mainLog.Info("API bind on custom port:", spec.ListenPort)
			}

			if converted, err := gw.kvStore(spec.Proxy.ListenPath); err == nil {
				spec.Proxy.ListenPath = converted
			}

			if currSpec := gw.getApiSpec(spec.APIID); !shouldReloadSpec(currSpec, spec) {
				tmpSpecRegister[spec.APIID] = currSpec
			} else {
				tmpSpecRegister[spec.APIID] = spec
			}

			switch spec.Protocol {
			case "", "http", "https", "h2c":
				if shouldTrace {
					// opentracing works only with http services.
					err := trace.AddTracer("", spec.Name)
					if err != nil {
						mainLog.Errorf("Failed to initialize tracer for %q error:%v", spec.Name, err)
					} else {
						mainLog.Infof("Intialized tracer  api_name=%q", spec.Name)
					}
				}
				tmpSpecHandle, err := gw.loadHTTPService(spec, apisByListen, &gs, muxer)
				if err != nil {
					log.WithError(err).Errorf("error loading API")
					return
				}
				tmpSpecHandles.Store(spec.APIID, tmpSpecHandle)
			case "tcp", "tls":
				gw.loadTCPService(spec, &gs, muxer)
			}

			// Set versions free to update links below
			spec.VersionDefinition.BaseID = ""
		}()
	}

	gw.DefaultProxyMux.swap(muxer, gw)

	var specsToUnload []*APISpec

	gw.apisMu.Lock()

	for _, spec := range specs {
		curSpec, ok := gw.apisByID[spec.APIID]
		if ok && curSpec != nil && shouldReloadSpec(curSpec, spec) {
			mainLog.Debugf("Spec %s has changed and needs to be reloaded", curSpec.APIID)
			specsToUnload = append(specsToUnload, curSpec)
		}

		// Bind versions to base APIs again
		for _, vID := range spec.VersionDefinition.Versions {
			if versionAPI, ok := tmpSpecRegister[vID]; ok {
				versionAPI.VersionDefinition.BaseID = spec.APIID
			}
		}
	}

	// Find the removed specs to unload them
	for apiID, curSpec := range gw.apisByID {
		if _, ok := tmpSpecRegister[apiID]; !ok {
			specsToUnload = append(specsToUnload, curSpec)
		}
	}

	gw.apisByID = tmpSpecRegister
	gw.apisHandlesByID = tmpSpecHandles

	gw.apisMu.Unlock()

	for _, spec := range specsToUnload {
		mainLog.Debugf("Unloading spec %s", spec.APIID)
		spec.Unload()
	}

	mainLog.Debug("Checker host list")

	// Kick off our host checkers
	if !gw.GetConfig().UptimeTests.Disable {
		gw.SetCheckerHostList()
	}

	mainLog.Debug("Checker host Done")

	mainLog.Info("Initialised API Definitions")

	gwListenPort := gw.GetConfig().ListenPort
	controlApiIsConfigured := (gw.GetConfig().ControlAPIPort != 0 && gw.GetConfig().ControlAPIPort != gwListenPort) || gw.GetConfig().ControlAPIHostname != ""

	if !gw.isRunningTests() && gw.allApisAreMTLS() && !gw.GetConfig().Security.ControlAPIUseMutualTLS && !controlApiIsConfigured {
		mainLog.Warning("All APIs are protected with mTLS, except for the control API. " +
			"We recommend configuring the control API port or control hostname to ensure consistent security measures")
	}
}

func recoverFromLoadApiPanic(spec *APISpec, err any) error {
	if spec.APIDefinition.IsOAS && spec.OAS.GetTykExtension() == nil {
		return fmt.Errorf("trying to import invalid OAS api %s, skipping", spec.APIID)
	}
	return fmt.Errorf("Panic while loading an API: %v, panic: %v, stacktrace: %v", spec.APIDefinition, err, string(debug.Stack()))
}

func (gw *Gateway) allApisAreMTLS() bool {
	gw.apisMu.RLock()
	defer gw.apisMu.RUnlock()
	for _, api := range gw.apisByID {
		if !api.UseMutualTLSAuth && api.Active {
			return false
		}
	}

	return true
}

// WithQuotaKey overrides quota key manually
func WithQuotaKey(key string) option.Option[ProcessSpecOptions] {
	return func(p *ProcessSpecOptions) {
		p.quotaKey = key
	}
}
