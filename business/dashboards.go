package business

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kiali/k-charted/config"
	"github.com/kiali/k-charted/kubernetes"
	"github.com/kiali/k-charted/kubernetes/v1alpha1"
	"github.com/kiali/k-charted/log"
	"github.com/kiali/k-charted/model"
	"github.com/kiali/k-charted/prometheus"
)

const defaultNamespaceLabel = "namespace"

// DashboardsService deals with fetching dashboards from k8s client
type DashboardsService struct {
	promClient prometheus.ClientInterface
	k8sClient  kubernetes.ClientInterface
	config     config.Config
	Logger     log.SafeAdapter
}

// NewDashboardsService initializes this business service
func NewDashboardsService(conf config.Config, logger log.LogAdapter) DashboardsService {
	return DashboardsService{
		config: conf,
		Logger: log.NewSafeAdapter(logger),
	}
}

func (in *DashboardsService) prom() (prometheus.ClientInterface, error) {
	// Lazy init
	if in.promClient == nil {
		client, err := prometheus.NewClient(in.config.Prometheus)
		if err != nil {
			return nil, fmt.Errorf("cannot initialize Prometheus Client: %v", err)
		}
		in.promClient = client
	}
	return in.promClient, nil
}

func (in *DashboardsService) k8s() (kubernetes.ClientInterface, error) {
	// Lazy init
	if in.k8sClient == nil {
		client, err := kubernetes.NewClient()
		if err != nil {
			return nil, fmt.Errorf("cannot initialize Kubernetes Client: %v", err)
		}
		in.k8sClient = client
	}
	return in.k8sClient, nil
}

// k8sGetDashboard wraps KialiMonitoringInterface.GetDashboard with client creation & error handling
func (in *DashboardsService) k8sGetDashboard(namespace, template string) (*v1alpha1.MonitoringDashboard, error) {
	client, err := in.k8s()
	if err != nil {
		return nil, err
	}
	in.Logger.Tracef("load k8s dashboard '%s' in namespace '%s'", template, namespace)
	return client.GetDashboard(namespace, template)
}

// k8sGetDashboards wraps KialiMonitoringInterface.GetDashboards with client creation & error handling
func (in *DashboardsService) k8sGetDashboards(namespace string) ([]v1alpha1.MonitoringDashboard, error) {
	client, err := in.k8s()
	if err != nil {
		return nil, err
	}
	in.Logger.Tracef("load all k8s dashboards in namespace '%s'", namespace)
	return client.GetDashboards(namespace)
}

func (in *DashboardsService) loadRawDashboardResource(namespace, template string) (*v1alpha1.MonitoringDashboard, error) {
	// There is an override mechanism with dashboards: default dashboards can be provided in Kiali namespace,
	// and can be overriden in app namespace.
	// So we look for the one in app namespace first, and only if not found fallback to the one in istio-system.
	dashboard, err := in.k8sGetDashboard(namespace, template)
	if err != nil && in.config.GlobalNamespace != "" {
		dashboard, err = in.k8sGetDashboard(in.config.GlobalNamespace, template)
		if err != nil {
			return nil, err
		}
	}
	return dashboard, err
}

func (in *DashboardsService) loadRawDashboardResources(namespace string) (map[string]v1alpha1.MonitoringDashboard, error) {
	all := make(map[string]v1alpha1.MonitoringDashboard)

	// From global namespace
	if in.config.GlobalNamespace != "" {
		dashboards, err := in.k8sGetDashboards(in.config.GlobalNamespace)
		if err != nil {
			return nil, err
		}
		for _, d := range dashboards {
			all[d.Name] = d
		}
	}

	// From specific namespace
	if namespace != in.config.GlobalNamespace {
		dashboards, err := in.k8sGetDashboards(namespace)
		if err != nil {
			return nil, err
		}
		for _, d := range dashboards {
			all[d.Name] = d
		}
	}

	return all, nil
}

func (in *DashboardsService) loadAndResolveDashboardResource(namespace, template string, loaded map[string]bool) (*v1alpha1.MonitoringDashboard, error) {
	// Circular dependency check
	if _, ok := loaded[template]; ok {
		return nil, fmt.Errorf("cannot load dashboard %s due to circular dependency detected. Already loaded dependencies: %v", template, loaded)
	}
	loaded[template] = true
	dashboard, err := in.loadRawDashboardResource(namespace, template)
	if err != nil {
		return nil, err
	}
	err = in.resolveReferences(namespace, dashboard, loaded)
	return dashboard, err
}

// resolveReferences resolves the composition mechanism that allows to reference a dashboard from another one
func (in *DashboardsService) resolveReferences(namespace string, dashboard *v1alpha1.MonitoringDashboard, loaded map[string]bool) error {
	resolved := []v1alpha1.MonitoringDashboardItem{}
	for _, item := range dashboard.Spec.Items {
		reference := strings.TrimSpace(item.Include)
		if reference != "" {
			// reference can point to a whole dashboard (ex: microprofile-1.0) or a chart within a dashboard (ex: microprofile-1.0$Thread count)
			parts := strings.Split(reference, "$")
			dashboardRefName := parts[0]
			composedDashboard, err := in.loadAndResolveDashboardResource(namespace, dashboardRefName, loaded)
			if err != nil {
				return err
			}
			for _, item2 := range composedDashboard.Spec.Items {
				if len(parts) > 1 {
					// Reference a specific chart
					if item2.Chart.Name == parts[1] {
						resolved = append(resolved, item2)
						break
					}
				} else {
					// Reference the whole dashboard
					resolved = append(resolved, item2)
				}
			}
		} else {
			resolved = append(resolved, item)
		}
	}
	dashboard.Spec.Items = resolved
	return nil
}

// GetDashboard returns a dashboard filled-in with target data
func (in *DashboardsService) GetDashboard(params model.DashboardQuery, template string) (*model.MonitoringDashboard, error) {
	promClient, err := in.prom()
	if err != nil {
		return nil, err
	}
	dashboard, err := in.loadAndResolveDashboardResource(params.Namespace, template, map[string]bool{})
	if err != nil {
		return nil, err
	}

	filters := in.buildLabels(params.Namespace, params.LabelsFilters)
	aggLabels := append(params.AdditionalLabels, model.ConvertAggregations(dashboard.Spec)...)
	if len(aggLabels) == 0 {
		// Prevent null in json
		aggLabels = []model.Aggregation{}
	}

	wg := sync.WaitGroup{}
	wg.Add(len(dashboard.Spec.Items) + 1)
	filledCharts := make([]model.Chart, len(dashboard.Spec.Items))

	for i, item := range dashboard.Spec.Items {
		go func(idx int, chart v1alpha1.MonitoringDashboardChart) {
			defer wg.Done()
			conversionParams := model.ConversionParams{Scale: 1.0, SortLabel: chart.SortLabel, SortLabelParseAs: chart.SortLabelParseAs}
			if chart.UnitScale != 0.0 {
				conversionParams.Scale = chart.UnitScale
			}
			// Group by labels is concat of what is defined in CR + what is passed as parameters
			byLabels := append(chart.GroupLabels, params.ByLabels...)
			if len(chart.SortLabel) > 0 {
				// We also need to group by the label used for sorting, if not explicitly present
				present := false
				for _, lbl := range byLabels {
					if lbl == chart.SortLabel {
						present = true
						break
					}
				}
				if !present {
					byLabels = append(byLabels, chart.SortLabel)
					// Mark the sort label to not be kept during conversion
					conversionParams.RemoveSortLabel = true
				}
			}
			grouping := strings.Join(byLabels, ",")

			filledCharts[idx] = model.ConvertChart(chart)
			metrics := chart.GetMetrics()
			for _, ref := range metrics {
				if chart.DataType == v1alpha1.Raw {
					aggregator := params.RawDataAggregator
					if chart.Aggregator != "" {
						aggregator = chart.Aggregator
					}
					metric := promClient.FetchRange(ref.MetricName, filters, grouping, aggregator, &params.MetricsQuery)
					filledCharts[idx].FillMetric(ref, metric, conversionParams)
				} else if chart.DataType == v1alpha1.Rate {
					metric := promClient.FetchRateRange(ref.MetricName, filters, grouping, &params.MetricsQuery)
					filledCharts[idx].FillMetric(ref, metric, conversionParams)
				} else {
					histo := promClient.FetchHistogramRange(ref.MetricName, filters, grouping, &params.MetricsQuery)
					filledCharts[idx].FillHistogram(ref, histo, conversionParams)
				}
			}
		}(i, item.Chart)
	}

	var externalLinks []model.ExternalLink
	go func() {
		defer wg.Done()
		links, _, err := GetGrafanaLinks(in.Logger, in.config.Grafana, dashboard.Spec.ExternalLinks)
		if err != nil {
			in.Logger.Errorf("Error while getting Grafana links: %v", err)
		}
		if links != nil {
			externalLinks = links
		} else {
			externalLinks = []model.ExternalLink{}
		}
	}()

	wg.Wait()
	return &model.MonitoringDashboard{
		Title:         dashboard.Spec.Title,
		Charts:        filledCharts,
		Aggregations:  aggLabels,
		ExternalLinks: externalLinks,
	}, nil
}

// SearchExplicitDashboards will check annotations of all supplied pods to extract a unique list of dashboards
//	Accepted annotations are "kiali.io/runtimes" and "kiali.io/dashboards"
func (in *DashboardsService) SearchExplicitDashboards(namespace string, pods []model.Pod) []model.Runtime {
	uniqueRefsList := extractUniqueDashboards(pods)
	if len(uniqueRefsList) > 0 {
		in.Logger.Tracef("getting dashboards from refs list: %v", uniqueRefsList)
		return in.buildRuntimesList(namespace, uniqueRefsList)
	}
	return []model.Runtime{}
}

func (in *DashboardsService) buildRuntimesList(namespace string, templatesNames []string) []model.Runtime {
	dashboards := make([]*v1alpha1.MonitoringDashboard, len(templatesNames))
	wg := sync.WaitGroup{}
	wg.Add(len(templatesNames))
	for idx, template := range templatesNames {
		go func(i int, tpl string) {
			defer wg.Done()
			dashboard, err := in.loadRawDashboardResource(namespace, tpl)
			if err != nil {
				in.Logger.Errorf("cannot get dashboard %s in namespace %s. Error was: %v", tpl, namespace, err)
			} else {
				dashboards[i] = dashboard
			}
		}(idx, template)
	}

	wg.Wait()

	runtimes := []model.Runtime{}
	for _, dashboard := range dashboards {
		if dashboard == nil {
			continue
		}
		runtimes = addDashboardToRuntimes(dashboard, runtimes)
	}
	return runtimes
}

func (in *DashboardsService) fetchMetricNames(namespace string, labelsFilters map[string]string) []string {
	promClient, err := in.prom()
	if err != nil {
		return []string{}
	}

	labels := in.buildLabels(namespace, labelsFilters)
	metrics, err := promClient.GetMetricsForLabels([]string{labels})
	if err != nil {
		in.Logger.Errorf("runtimes discovery failed, cannot load metrics for labels: %s. Error was: %v", labels, err)
	}
	return metrics
}

// DiscoverDashboards tries to discover dashboards based on existing metrics
func (in *DashboardsService) DiscoverDashboards(namespace string, labelsFilters map[string]string) []model.Runtime {
	in.Logger.Tracef("starting runtimes discovery on namespace %s with filters [%v]", namespace, labelsFilters)

	var metrics []string
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		metrics = in.fetchMetricNames(namespace, labelsFilters)
	}()

	allDashboards, err := in.loadRawDashboardResources(namespace)
	if err != nil {
		in.Logger.Errorf("runtimes discovery failed, cannot load dashboards in namespace %s. Error was: %v", namespace, err)
		return []model.Runtime{}
	}

	wg.Wait()
	return runDiscoveryMatcher(metrics, allDashboards)
}

func runDiscoveryMatcher(metrics []string, allDashboards map[string]v1alpha1.MonitoringDashboard) []model.Runtime {
	// In all dashboards, finds the ones that match the metrics set
	// We must exclude from the results included dashboards when both the including and the included dashboards are matching
	runtimesMap := make(map[string]*v1alpha1.MonitoringDashboard)
	for _, d := range allDashboards {
		dashboard := d // sticky reference
		matchReference := strings.TrimSpace(dashboard.Spec.DiscoverOn)
		if matchReference != "" {
			for _, metric := range metrics {
				if matchReference == strings.TrimSpace(metric) {
					if _, exists := runtimesMap[dashboard.Name]; !exists {
						runtimesMap[dashboard.Name] = &dashboard
					}
					// Mark included dashboards as already found
					// and set them "nil" to not show them as standalone dashboards even if they match
					for _, item := range dashboard.Spec.Items {
						if item.Include != "" {
							runtimesMap[item.Include] = nil
						}
					}
					break
				}
			}
		}
	}
	runtimes := []model.Runtime{}
	for _, dashboard := range runtimesMap {
		if dashboard != nil {
			runtimes = addDashboardToRuntimes(dashboard, runtimes)
		}
	}
	sort.Slice(runtimes, func(i, j int) bool { return runtimes[i].Name < runtimes[j].Name })
	return runtimes
}

func addDashboardToRuntimes(dashboard *v1alpha1.MonitoringDashboard, runtimes []model.Runtime) []model.Runtime {
	runtime := dashboard.Spec.Runtime
	ref := model.DashboardRef{
		Template: dashboard.Name,
		Title:    dashboard.Spec.Title,
	}
	found := false
	for i := range runtimes {
		rtObj := &runtimes[i]
		if rtObj.Name == runtime {
			rtObj.DashboardRefs = append(rtObj.DashboardRefs, ref)
			found = true
			break
		}
	}
	if !found {
		runtimes = append(runtimes, model.Runtime{
			Name:          runtime,
			DashboardRefs: []model.DashboardRef{ref},
		})
	}
	return runtimes
}

func (in *DashboardsService) buildLabels(namespace string, labelsFilters map[string]string) string {
	namespaceLabel := in.config.NamespaceLabel
	if namespaceLabel == "" {
		namespaceLabel = defaultNamespaceLabel
	}
	labels := fmt.Sprintf(`{%s="%s"`, namespaceLabel, namespace)
	for k, v := range labelsFilters {
		labels += fmt.Sprintf(`,%s="%s"`, k, v)
	}
	labels += "}"
	return labels
}
