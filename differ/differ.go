// Copyright 2021 Red Hat, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package differ

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/RedHatInsights/ccx-notification-service/conf"
	"github.com/RedHatInsights/ccx-notification-service/producer"
	"github.com/RedHatInsights/ccx-notification-service/types"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Configuration-related constants
const (
	configFileEnvVariableName = "NOTIFICATION_SERVICE_CONFIG_FILE"
	defaultConfigFileName     = "config"

	// TODO: make this configurable via config file
	totalRiskThreshold = 3
)

// Exit codes
const (
	// ExitStatusOK means that the tool finished with success
	ExitStatusOK = iota
	// ExitStatusConfiguration is an error code related to program configuration
	ExitStatusConfiguration
	// ExitStatusError is a general error code
	ExitStatusError
	// ExitStatusStorageError is returned in case of any consumer-related error
	ExitStatusStorageError
	// ExitStatusFetchContentError is returned in case content cannot be fetch correctly
	ExitStatusFetchContentError
	// ExitStatusKafkaBrokerError is for kafka broker connection establishment errors
	ExitStatusKafkaBrokerError
	// ExitStatusKafkaProducerError is for kafka event production failures
	ExitStatusKafkaProducerError
	// ExitStatusKafkaConnectionNotClosedError is raised when connection cannot be closed
	ExitStatusKafkaConnectionNotClosedError
)

// Messages
const (
	versionMessage              = "Notification writer version 1.0"
	authorsMessage              = "Pavel Tisnovsky, Red Hat Inc."
	separator                   = "------------------------------------------------------------"
	operationFailedMessage      = "Operation failed"
	clusterEntryMessage         = "cluster entry"
	organizationIDAttribute     = "org id"
	AccountNumberAttribute      = "account number"
	clusterAttribute            = "cluster"
	totalRiskAttribute          = "totalRisk"
	errorStr                    = "Error:"
	invalidJSONContent          = "The provided content cannot be encoded as JSON."
	contextToEscapedStringError = "Notification message will not be generated as context couldn't be converted to escaped string."
)

// Constants for notification message top level fields
const (
	notificationBundleName      = "openshift"
	notificationApplicationName = "advisor"
)

// Constants for notification event expected fields
const (
	//INSTANT NOTIFICATION PAYLOAD FIELDS
	notificationPayloadRuleDescription = "rule_description"
	notificationPayloadRuleURL         = "rule_url"
	notificationPayloadTotalRisk       = "total_risk"
	notificationPayloadPublishDate     = "publish_date"
	//WEEKLY NOTIFICATION PAYLOAD FIELDS
	notificationPayloadTotalClusters        = "total_clusters"
	notificationPayloadTotalRecommendations = "total_recommendations"
	notificationPayloadTotalCritical        = "total_critical"
	notificationPayloadTotalImportant       = "total_important"
	notificationPayloadTotalIncidents       = "total_incidents"
)

// Constants for notification context expected fields
const (
	notificationContextDisplayName = "display_name"
	notificationContextHostURL     = "host_url"
	notificationContextAdvisorURL  = "advisor_url"
)

var (
	notificationType types.EventType
)

// showVersion function displays version information.
func showVersion() {
	fmt.Println(versionMessage)
}

// showAuthors function displays information about authors.
func showAuthors() {
	fmt.Println(authorsMessage)
}

func calculateTotalRisk(impact, likelihood int) int {
	return (impact + likelihood) / 2
}

// ccx_rules_ocp.external.rules.cluster_wide_proxy_auth_check.report
// ->
// cluster_wide_proxy_auth_check
func moduleToRuleName(module string) string {
	result := strings.TrimSuffix(module, ".report")
	result = strings.TrimPrefix(result, "ccx_rules_ocp.")
	result = strings.TrimPrefix(result, "external.")
	result = strings.TrimPrefix(result, "rules.")
	result = strings.TrimPrefix(result, "bug_rules.")
	result = strings.TrimPrefix(result, "ocs.")

	return result
}

func findRuleByNameAndErrorKey(ruleContent map[string]types.RuleContent, impacts types.Impacts, ruleName string, errorKey string) (int, int, int) {
	rc := ruleContent[ruleName]
	ek := rc.ErrorKeys
	val := ek[errorKey]
	likelihood := val.Metadata.Likelihood
	impact := impacts[val.Metadata.Impact]
	totalRisk := calculateTotalRisk(likelihood, impact)
	return val.Metadata.Likelihood, impact, totalRisk
}

func waitForEnter() {
	fmt.Println("\n... demo mode ... Press 'Enter' to continue...")
	_, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
	if err != nil {
		log.Error().Err(err)
	}
}

func processReportsByCluster(ruleContent map[string]types.RuleContent, impacts types.Impacts, storage *DBStorage, clusters []types.ClusterEntry, notificationConfig conf.NotificationsConfiguration, notifier *producer.KafkaProducer) {
	for i, cluster := range clusters {
		log.Info().
			Int("#", i).
			Int(organizationIDAttribute, int(cluster.OrgID)).
			Int(AccountNumberAttribute, int(cluster.AccountNumber)).
			Str(clusterAttribute, string(cluster.ClusterName)).
			Msg(clusterEntryMessage)

		report, reportedAt, err := storage.ReadReportForCluster(cluster.OrgID, cluster.ClusterName)
		if err != nil {
			log.Err(err).Msg(operationFailedMessage)
			os.Exit(ExitStatusStorageError)
		}

		var deserialized types.Report
		err = json.Unmarshal([]byte(report), &deserialized)
		if err != nil {
			log.Err(err).Msg("Deserialization error - Couldn't create report object")
			os.Exit(ExitStatusStorageError)
		}

		if len(deserialized.Reports) == 0 {
			log.Info().Msgf("No reports in notification database for cluster %s", cluster.ClusterName)
			continue
		}

		notificationMsg := generateInstantNotificationMessage(notificationConfig.ClusterDetailsURI, fmt.Sprint(cluster.AccountNumber), string(cluster.ClusterName))

		for i, r := range deserialized.Reports {
			ruleName := moduleToRuleName(string(r.Module))
			errorKey := string(r.ErrorKey)
			likelihood, impact, totalRisk := findRuleByNameAndErrorKey(ruleContent, impacts, ruleName, errorKey)

			log.Info().
				Int("#", i).
				Str("type", r.Type).
				Str("rule", ruleName).
				Str("error key", errorKey).
				Int("likelihood", likelihood).
				Int("impact", impact).
				Int(totalRiskAttribute, totalRisk).
				Msg("Report")
			if totalRisk >= totalRiskThreshold {
				log.Warn().Int(totalRiskAttribute, totalRisk).Msg("Report with high impact detected")
				appendEventToNotificationMessage(notificationConfig.RuleDetailsURI, &notificationMsg, ruleName, totalRisk, time.Time(reportedAt).UTC().Format(time.RFC3339Nano))
			}
		}

		if len(notificationMsg.Events) == 0 {
			log.Info().Msgf("No new issues to notify for cluster %s", string(cluster.ClusterName))
			continue
		}

		log.Info().Msgf("Producing instant notification for cluster %s with %d events", string(cluster.ClusterName), len(notificationMsg.Events))
		_, _, err = notifier.ProduceMessage(notificationMsg)
		if err != nil {
			log.Error().
				Str(errorStr, err.Error()).
				Msg("Couldn't produce kafka event.")
			os.Exit(ExitStatusKafkaProducerError)
		}
	}
}

// getNotificationDigestForCurrentAccount function returns new digest object if none has been previously created for given account
func getNotificationDigestForCurrentAccount(notificationsByAccount map[types.AccountNumber]types.Digest, accountNumber types.AccountNumber) (digest types.Digest) {
	if _, ok := notificationsByAccount[accountNumber]; !ok {
		log.Info().Msgf("Creating notification digest for account %d", accountNumber)
		digest = types.Digest{}
	} else {
		log.Info().Msgf("Modifying notification digest for account %d", accountNumber)
		digest = notificationsByAccount[accountNumber]
	}
	return
}

// updateDigestNotificationCounters function increments number of important or critical notification detected in a given weekly digest
func updateDigestNotificationCounters(digest *types.Digest, totalRisk int) {
	if totalRisk == 3 {
		log.Warn().Int(totalRiskAttribute, totalRisk).Msg("Important report detected. Adding to weekly digest")
		digest.ImportantNotifications++
	} else if totalRisk == 4 {
		log.Warn().Int(totalRiskAttribute, totalRisk).Msg("Critical report detected. Adding to weekly digest")
		digest.CriticalNotifications++
	}
}

// processAllReportsFromCurrentWeek function creates weekly digest with for all the clusters corresponding to each user account
func processAllReportsFromCurrentWeek(ruleContent map[string]types.RuleContent, impacts types.Impacts, storage *DBStorage, clusters []types.ClusterEntry, notificationConfig conf.NotificationsConfiguration, notifier *producer.KafkaProducer) {
	digestByAccount := map[types.AccountNumber]types.Digest{}
	digest := types.Digest{}

	for i, cluster := range clusters {
		log.Info().
			Int("#", i).
			Int(organizationIDAttribute, int(cluster.OrgID)).
			Int(AccountNumberAttribute, int(cluster.AccountNumber)).
			Str(clusterAttribute, string(cluster.ClusterName)).
			Msg(clusterEntryMessage)

		digest = getNotificationDigestForCurrentAccount(digestByAccount, cluster.AccountNumber)
		digest.ClustersAffected++

		report, _, err := storage.ReadReportForCluster(cluster.OrgID, cluster.ClusterName)
		if err != nil {
			log.Err(err).Msg(operationFailedMessage)
			os.Exit(ExitStatusStorageError)
		}

		var deserialized types.Report
		err = json.Unmarshal([]byte(report), &deserialized)
		if err != nil {
			log.Err(err).Msg("Deserialization error - Couldn't create report object")
			os.Exit(ExitStatusStorageError)
		}

		numReports := len(deserialized.Reports)
		if numReports == 0 {
			log.Info().Msgf("No reports in notification database for cluster %s", cluster.ClusterName)
			continue
		}
		digest.Recommendations += numReports

		for i, r := range deserialized.Reports {
			ruleName := moduleToRuleName(string(r.Module))
			errorKey := string(r.ErrorKey)
			likelihood, impact, totalRisk := findRuleByNameAndErrorKey(ruleContent, impacts, ruleName, errorKey)

			log.Info().
				Int("#", i).
				Str("type", r.Type).
				Str("rule", ruleName).
				Str("error key", errorKey).
				Int("likelihood", likelihood).
				Int("impact", impact).
				Int(totalRiskAttribute, totalRisk).
				Msg("Report")
			updateDigestNotificationCounters(&digest, totalRisk)
		}
		digestByAccount[cluster.AccountNumber] = digest
	}

	for account, digest := range digestByAccount {
		if digest.Recommendations == 0 {
			log.Info().Msgf("No issues to notify to account %d", account)
			continue
		}

		log.Info().
			Int("account number", int(account)).
			Int("total recommendations", digest.Recommendations).
			Int("clusters affected", digest.ClustersAffected).
			Int("critical notifications", digest.CriticalNotifications).
			Int("important notifications", digest.ImportantNotifications).
			Msg("Producing weekly notification for ")

		notification := generateWeeklyNotificationMessage(notificationConfig.InsightsAdvisorURL, fmt.Sprint(account), digest)
		_, _, err := notifier.ProduceMessage(notification)
		if err != nil {
			log.Error().
				Str(errorStr, err.Error()).
				Msg("Couldn't produce kafka event.")
			os.Exit(ExitStatusKafkaProducerError)
		}
	}
}

// processClusters function creates desired notification messages for all the clusters obtained from the database
func processClusters(ruleContent map[string]types.RuleContent, impacts types.Impacts, storage *DBStorage, clusters []types.ClusterEntry, config conf.ConfigStruct) {
	log.Info().Msg(separator)
	log.Info().Msg("Preparing Kafka producer")

	notifier := setupNotificationProducer(conf.GetKafkaBrokerConfiguration(config))
	log.Info().Msg("Kafka producer ready")

	waitForEnter()

	notificationConfig := conf.GetNotificationsConfiguration(config)

	if notificationType == types.InstantNotif {
		processReportsByCluster(ruleContent, impacts, storage, clusters, notificationConfig, notifier)
	} else if notificationType == types.WeeklyDigest {
		processAllReportsFromCurrentWeek(ruleContent, impacts, storage, clusters, notificationConfig, notifier)
	}

	err := notifier.Close()
	if err != nil {
		log.Error().
			Str(errorStr, err.Error()).
			Msg("Couldn't close Kafka connection.")
		os.Exit(ExitStatusKafkaConnectionNotClosedError)
	}
}

// printClusters function displays information of all clusters in the given list
func printClusters(clusters []types.ClusterEntry) {
	for i, cluster := range clusters {
		log.Info().
			Int("#", i).
			Int(organizationIDAttribute, int(cluster.OrgID)).
			Int(AccountNumberAttribute, int(cluster.AccountNumber)).
			Str(clusterAttribute, string(cluster.ClusterName)).
			Msg(clusterEntryMessage)
	}
}

// setupNotificationProducer function creates a kafka producer using the provided configuration
func setupNotificationProducer(brokerConfig conf.KafkaConfiguration) (notifier *producer.KafkaProducer) {
	notifier, err := producer.New(brokerConfig)
	if err != nil {
		log.Error().
			Str(errorStr, err.Error()).
			Msg("Couldn't initialize Kafka producer with the provided config.")
		os.Exit(ExitStatusKafkaBrokerError)
	}
	return
}

// generateInstantNotificationMessage function generates a notification message with no events for a given account+cluster
func generateInstantNotificationMessage(clusterURI string, accountID string, clusterID string) (notification types.NotificationMessage) {
	events := []types.Event{}
	context := toJSONEscapedString(types.NotificationContext{
		notificationContextDisplayName: clusterID,
		notificationContextHostURL:     strings.Replace(clusterURI, "{cluster}", clusterID, 1),
	})
	if context == "" {
		log.Error().Msg(contextToEscapedStringError)
		return
	}

	notification = types.NotificationMessage{
		Bundle:      notificationBundleName,
		Application: notificationApplicationName,
		EventType:   types.InstantNotif.String(),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		AccountID:   accountID,
		Events:      events,
		Context:     context,
	}
	return
}

// generateWeeklyNotificationMessage function generates a notification message with one event based on the provided digest
func generateWeeklyNotificationMessage(advisorURI string, accountID string, digest types.Digest) (notification types.NotificationMessage) {
	context := toJSONEscapedString(types.NotificationContext{
		notificationContextAdvisorURL: advisorURI,
	})
	if context == "" {
		log.Error().Msg(contextToEscapedStringError)
		return
	}

	payload := toJSONEscapedString(types.EventPayload{
		notificationPayloadTotalClusters:        fmt.Sprint(digest.ClustersAffected),
		notificationPayloadTotalRecommendations: fmt.Sprint(digest.Recommendations),
		notificationPayloadTotalIncidents:       fmt.Sprint(digest.Incidents),
		notificationPayloadTotalCritical:        fmt.Sprint(digest.CriticalNotifications),
		notificationPayloadTotalImportant:       fmt.Sprint(digest.ImportantNotifications),
	})
	if payload == "" {
		log.Error().Msg("Notification message will not be generated as payload couldn't be converted to escaped string.")
		return
	}

	events := []types.Event{
		{
			//The insights Notifications backend expects this field to be an empty object in the received JSON
			Metadata: types.EventMetadata{},
			//The insights Notifications backend expects to receive the payload as a string with all its fields as escaped strings
			Payload: payload,
		},
	}

	notification = types.NotificationMessage{
		Bundle:      notificationBundleName,
		Application: notificationApplicationName,
		EventType:   types.WeeklyDigest.String(),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		AccountID:   accountID,
		Events:      events,
		Context:     context,
	}
	return
}

// appendEventToNotificationMessage function adds a new event to the given notification message after constructing the payload string
func appendEventToNotificationMessage(ruleURI string, notification *types.NotificationMessage, ruleName string, totalRisk int, publishDate string) {
	payload := toJSONEscapedString(types.EventPayload{
		notificationPayloadRuleDescription: ruleName,
		notificationPayloadRuleURL:         strings.Replace(ruleURI, "{rule}", ruleName, 1),
		notificationPayloadTotalRisk:       fmt.Sprint(totalRisk),
		notificationPayloadPublishDate:     publishDate,
	})
	if payload == "" {
		log.Error().Msg(contextToEscapedStringError)
		return
	}
	event := types.Event{
		//The insights Notifications backend expects this field to be an empty object in the received JSON
		Metadata: types.EventMetadata{},
		//The insights Notifications backend expects to receive the payload as a string with all its fields as escaped strings
		Payload: payload,
	}
	notification.Events = append(notification.Events, event)
}

// toJSONEscapedString function turns any valid JSON to a string-escaped string
func toJSONEscapedString(i interface{}) string {
	b, err := json.Marshal(i)
	if err != nil {
		log.Err(err).Msg(invalidJSONContent)
	}
	s := string(b)
	return s
}

// checkArgs function handles command line options passed to the process
func checkArgs(args *types.CliFlags) {
	switch {
	case args.ShowVersion:
		showVersion()
		os.Exit(ExitStatusOK)
	case args.ShowAuthors:
		showAuthors()
		os.Exit(ExitStatusOK)
	default:
	}

	// check if report type is specified on command line
	if !args.InstantReports && !args.WeeklyReports {
		log.Error().Msg("Type of report needs to be specified on command line")
		os.Exit(ExitStatusConfiguration)
	}

	if args.InstantReports {
		notificationType = types.InstantNotif
	} else {
		notificationType = types.WeeklyDigest
	}
}

// Run function is entry point to the differ
func Run() {
	var cliFlags types.CliFlags

	// define and parse all command line options
	flag.BoolVar(&cliFlags.InstantReports, "instant-reports", false, "create instant reports")
	flag.BoolVar(&cliFlags.WeeklyReports, "weekly-reports", false, "create weekly reports")
	flag.BoolVar(&cliFlags.ShowVersion, "show-version", false, "show version and exit")
	flag.BoolVar(&cliFlags.ShowAuthors, "show-authors", false, "show authors and exit")
	flag.Parse()
	checkArgs(&cliFlags)

	// config has exactly the same structure as *.toml file
	config, err := conf.LoadConfiguration(configFileEnvVariableName, defaultConfigFileName)
	if err != nil {
		log.Err(err).Msg("Load configuration")
		os.Exit(ExitStatusConfiguration)
	}

	if config.Logging.Debug {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	log.Info().Msg("Differ started")
	waitForEnter()

	log.Info().Msg(separator)

	log.Info().Msg("Getting rule content and impacts from content service")

	ruleContent, impacts, err := fetchAllRulesContent(conf.GetDependenciesConfiguration(config))
	if err != nil {
		os.Exit(ExitStatusFetchContentError)
	}

	waitForEnter()

	log.Info().Msg(separator)
	log.Info().Msg("Read cluster list")

	// prepare the storage
	storageConfiguration := conf.GetStorageConfiguration(config)
	storage, err := NewStorage(storageConfiguration)
	if err != nil {
		log.Err(err).Msg(operationFailedMessage)
		os.Exit(ExitStatusStorageError)
	}

	//TODO: Set notificationConfig global variables here to avoid passing so much parameters

	clusters, err := storage.ReadClusterList()
	if err != nil {
		log.Err(err).Msg(operationFailedMessage)
		os.Exit(ExitStatusStorageError)
	}

	printClusters(clusters)
	log.Info().Int("clusters", len(clusters)).Msg("Read cluster list: done")
	waitForEnter()

	log.Info().Msg(separator)
	log.Info().Msg("Checking new issues for all new reports")
	waitForEnter()

	processClusters(ruleContent, impacts, storage, clusters, config)

	log.Info().Msg("Differ finished")
}
