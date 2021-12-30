package msgservice

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/aquasecurity/postee/data"
	"github.com/aquasecurity/postee/dbservice"
	"github.com/aquasecurity/postee/log"
	"github.com/aquasecurity/postee/outputs"
	"github.com/aquasecurity/postee/regoservice"
	"github.com/aquasecurity/postee/routes"
	"github.com/aquasecurity/postee/utils"
)

type MsgService struct {
}

func (scan *MsgService) MsgHandling(in map[string]interface{}, output outputs.Output, route *routes.InputRoute, inpteval data.Inpteval, AquaServer *string) {
	if output == nil {
		return
	}

	//TODO marshalling message back to bytes, change after merge with https://github.com/aquasecurity/postee/pull/150
	input, _ := json.Marshal(in)

	if ok, err := regoservice.DoesMatchRegoCriteria(in, route.InputFiles, route.Input); err != nil {
		if !regoservice.IsUsedRegoFiles(route.InputFiles) {
			utils.PrnInputLogs("Error while evaluating rego rule %s :%v for the input %s", route.Input, err, input)
		} else {
			utils.PrnInputLogs("Error while evaluating rego rule for input files :%v for the input %s", err, input)
		}
		return
	} else if !ok {
		if !regoservice.IsUsedRegoFiles(route.InputFiles) {
			utils.PrnInputLogs("Input %s... doesn't match a REGO rule: %s", input, route.Input)
		} else {
			utils.PrnInputLogs("Input %s... doesn't match a REGO input files rule", input)
		}
		return
	}

	//TODO move logic below somewhere close to Jira output implementation
	owners := ""
	applicationScopeOwnersObj, ok := in["application_scope_owners"]
	if ok {
		applicationScopeOwners := make([]string, 0)

		applicationScopeOwners = append(applicationScopeOwners, applicationScopeOwnersObj.([]string)...)

		if len(applicationScopeOwners) > 0 {
			owners = strings.Join(applicationScopeOwners, ";")
		}
	}

	if route.Plugins.UniqueMessageProps != nil && len(route.Plugins.UniqueMessageProps) > 0 {
		msgKey := GetMessageUniqueId(in, route.Plugins.UniqueMessageProps)
		expired := calculateExpired(route.Plugins.UniqueMessageTimeoutSeconds)

		wasStored, err := dbservice.Db.MayBeStoreMessage(input, msgKey, expired)
		if err != nil {
			log.Logger.Errorf("Error while storing input: %v", err)
			return
		}
		if !wasStored {
			log.Logger.Infof("The same message was received before: %s", msgKey)
			return
		}

	}

	posteeOpts := map[string]string{
		"AquaServer": *AquaServer,
	}

	in["postee"] = posteeOpts

	content, err := inpteval.Eval(in, *AquaServer)
	if err != nil {
		log.Logger.Errorf("Error while evaluating input: %v", err)
		return
	}

	if owners != "" {
		content["owners"] = owners
	}

	if route.Plugins.AggregateMessageNumber > 0 && inpteval.IsAggregationSupported() {
		aggregated := AggregateScanAndGetQueue(route.Name, content, route.Plugins.AggregateMessageNumber, false)
		if len(aggregated) > 0 {
			content, err = inpteval.BuildAggregatedContent(aggregated)
			if err != nil {
				log.Logger.Errorf("Error while building aggregated content: %v", err)
				return
			}
			send(output, content)
		}
	} else if route.Plugins.AggregateTimeoutSeconds > 0 && inpteval.IsAggregationSupported() {
		AggregateScanAndGetQueue(route.Name, content, 0, true)

		if !route.IsSchedulerRun() { //TODO route shouldn't have any associated logic
			log.Logger.Infof("about to schedule %s", route.Name)
			RunScheduler(route, send, AggregateScanAndGetQueue, inpteval, &route.Name, output)
		} else {
			log.Logger.Infof("%s is already scheduled", route.Name)
		}
	} else {
		send(output, content)

	}
}

func send(otpt outputs.Output, cnt map[string]string) {
	go func() {
		err := otpt.Send(cnt)
		if err != nil {
			log.Logger.Errorf("Error while sending event: %v", err)
		}
	}()

	err := dbservice.Db.RegisterPlgnInvctn(otpt.GetName())
	if err != nil {
		log.Logger.Errorf("Error while building aggregated content: %v", err)
		return
	}

}
func calculateExpired(UniqueMessageTimeoutSeconds int) *time.Time {
	if UniqueMessageTimeoutSeconds == 0 {
		return nil
	}
	timeToExpire := time.Duration(UniqueMessageTimeoutSeconds) * time.Second
	expired := time.Now().UTC().Add(timeToExpire)
	return &expired
}

var AggregateScanAndGetQueue = func(outputName string, currentContent map[string]string, counts int, ignoreLength bool) []map[string]string {
	aggregatedScans, err := dbservice.Db.AggregateScans(outputName, currentContent, counts, ignoreLength)
	if err != nil {
		log.Logger.Errorf("AggregateScans Error: %v", err)
		return aggregatedScans
	}
	if len(currentContent) != 0 && len(aggregatedScans) == 0 {
		log.Logger.Infof("New scan was added to the queue of %q without sending.", outputName)
		return nil
	}
	return aggregatedScans
}
