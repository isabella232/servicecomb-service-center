/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/apache/servicecomb-service-center/datasource/etcd/client"
	"github.com/apache/servicecomb-service-center/datasource/etcd/kv"
	"github.com/apache/servicecomb-service-center/pkg/log"
	pb "github.com/apache/servicecomb-service-center/pkg/registry"
	"github.com/apache/servicecomb-service-center/pkg/util"
	apt "github.com/apache/servicecomb-service-center/server/core"
	scerr "github.com/apache/servicecomb-service-center/server/scerror"
	"strings"
)

func GetConsumerIds(ctx context.Context, domainProject string, provider *pb.MicroService) ([]string, error) {
	// 查询所有consumer
	dr := NewProviderDependencyRelation(ctx, domainProject, provider)
	consumerIds, err := dr.GetDependencyConsumerIds()
	if err != nil {
		log.Errorf(err, "get service[%s]'s consumerIds failed", provider.ServiceId)
		return nil, err
	}
	return consumerIds, nil
}

func GetProviderIds(ctx context.Context, domainProject string, consumer *pb.MicroService) ([]string, error) {
	// 查询所有provider
	dr := NewConsumerDependencyRelation(ctx, domainProject, consumer)
	providerIDs, err := dr.GetDependencyProviderIds()
	if err != nil {
		log.Errorf(err, "get service[%s]'s providerIDs failed", consumer.ServiceId)
		return nil, err
	}
	return providerIDs, nil
}

// GetAllConsumerIds is the function get from dependency rule and filter with service rules
func GetAllConsumerIds(ctx context.Context, domainProject string, provider *pb.MicroService) (allow []string, deny []string, _ error) {
	if provider == nil || len(provider.ServiceId) == 0 {
		return nil, nil, fmt.Errorf("invalid provider")
	}

	//todo 删除服务，最后实例推送有误差
	providerRules, err := GetRulesUtil(ctx, domainProject, provider.ServiceId)
	if err != nil {
		return nil, nil, err
	}

	rf := &RuleFilter{
		DomainProject: domainProject,
		ProviderRules: providerRules,
	}

	allow, deny, err = GetConsumerIdsWithFilter(ctx, domainProject, provider, rf)
	if err != nil {
		return nil, nil, err
	}
	return allow, deny, nil
}

func GetConsumerIdsWithFilter(ctx context.Context, domainProject string, provider *pb.MicroService, rf *RuleFilter) (allow []string, deny []string, err error) {
	consumerIds, err := GetConsumerIds(ctx, domainProject, provider)
	if err != nil {
		return nil, nil, err
	}
	return rf.FilterAll(ctx, consumerIds)
}

func GetAllProviderIds(ctx context.Context, domainProject string, service *pb.MicroService) (allow []string, deny []string, _ error) {
	providerIDsInCache, err := GetProviderIds(ctx, domainProject, service)
	if err != nil {
		return nil, nil, err
	}
	l := len(providerIDsInCache)
	rf := RuleFilter{
		DomainProject: domainProject,
	}
	allowIdx, denyIdx := 0, l
	providerIDs := make([]string, l)
	copyCtx := util.SetContext(util.CloneContext(ctx), util.CtxCacheOnly, "1")
	for _, providerID := range providerIDsInCache {
		providerRules, err := GetRulesUtil(copyCtx, domainProject, providerID)
		if err != nil {
			return nil, nil, err
		}
		if len(providerRules) == 0 {
			providerIDs[allowIdx] = providerID
			allowIdx++
			continue
		}
		rf.ProviderRules = providerRules
		ok, err := rf.Filter(ctx, service.ServiceId)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			providerIDs[allowIdx] = providerID
			allowIdx++
		} else {
			denyIdx--
			providerIDs[denyIdx] = providerID
		}
	}
	return providerIDs[:allowIdx], providerIDs[denyIdx:], nil
}

func DependencyRuleExist(ctx context.Context, provider *pb.MicroServiceKey, consumer *pb.MicroServiceKey) (bool, error) {
	targetDomainProject := provider.Tenant
	if len(targetDomainProject) == 0 {
		targetDomainProject = consumer.Tenant
	}

	consumerKey := apt.GenerateConsumerDependencyRuleKey(consumer.Tenant, consumer)
	existed, err := DependencyRuleExistUtil(ctx, consumerKey, provider)
	if err != nil || existed {
		return existed, err
	}

	providerKey := apt.GenerateProviderDependencyRuleKey(targetDomainProject, provider)
	return DependencyRuleExistUtil(ctx, providerKey, consumer)
}

func DependencyRuleExistUtil(ctx context.Context, key string, target *pb.MicroServiceKey) (bool, error) {
	compareData, err := TransferToMicroServiceDependency(ctx, key)
	if err != nil {
		return false, err
	}
	if len(compareData.Dependency) != 0 {
		isEqual, err := ContainServiceDependency(compareData.Dependency, target)
		if err != nil {
			return false, err
		}
		if isEqual {
			//删除之前的依赖
			return true, nil
		}
	}
	return false, nil
}

func AddServiceVersionRule(ctx context.Context, domainProject string, consumer *pb.MicroService, provider *pb.MicroServiceKey) error {
	//创建依赖一致
	consumerKey := pb.MicroServiceToKey(domainProject, consumer)
	exist, err := DependencyRuleExist(ctx, provider, consumerKey)
	if exist || err != nil {
		return err
	}

	r := &pb.ConsumerDependency{
		Consumer:  consumerKey,
		Providers: []*pb.MicroServiceKey{provider},
		Override:  false,
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}

	id := util.StringJoin([]string{provider.AppId, provider.ServiceName}, "_")
	key := apt.GenerateConsumerDependencyQueueKey(domainProject, consumer.ServiceId, id)
	resp, err := client.Instance().TxnWithCmp(ctx,
		nil,
		[]client.CompareOp{client.OpCmp(client.CmpStrVal(key), client.CmpEqual, util.BytesToStringWithNoCopy(data))},
		[]client.PluginOp{client.OpPut(client.WithStrKey(key), client.WithValue(data))})
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		log.Infof("put in queue[%s/%s]: consumer[%s/%s/%s/%s] -> provider[%s/%s/%s/%s]", consumer.ServiceId, id,
			consumer.Environment, consumer.AppId, consumer.ServiceName, consumer.Version,
			provider.Environment, provider.AppId, provider.ServiceName, provider.Version)
	}
	return nil
}

func TransferToMicroServiceDependency(ctx context.Context, key string) (*pb.MicroServiceDependency, error) {
	microServiceDependency := &pb.MicroServiceDependency{
		Dependency: []*pb.MicroServiceKey{},
	}

	opts := append(FromContext(ctx), client.WithStrKey(key))
	res, err := kv.Store().DependencyRule().Search(ctx, opts...)
	if err != nil {
		log.Errorf(nil, "get dependency rule[%s] failed", key)
		return nil, err
	}
	if len(res.Kvs) != 0 {
		return res.Kvs[0].Value.(*pb.MicroServiceDependency), nil
	}
	return microServiceDependency, nil
}

func EqualServiceDependency(serviceA *pb.MicroServiceKey, serviceB *pb.MicroServiceKey) bool {
	stringA := toString(serviceA)
	stringB := toString(serviceB)
	return stringA == stringB
}

func DiffServiceVersion(serviceA *pb.MicroServiceKey, serviceB *pb.MicroServiceKey) bool {
	stringA := toString(serviceA)
	stringB := toString(serviceB)
	if stringA != stringB &&
		stringA[:strings.LastIndex(stringA, "/")+1] == stringB[:strings.LastIndex(stringB, "/")+1] {
		return true
	}
	return false
}

func toString(in *pb.MicroServiceKey) string {
	return apt.GenerateProviderDependencyRuleKey(in.Tenant, in)
}

func parseAddOrUpdateRules(ctx context.Context, dep *Dependency) (createDependencyRuleList, existDependencyRuleList, deleteDependencyRuleList []*pb.MicroServiceKey) {
	conKey := apt.GenerateConsumerDependencyRuleKey(dep.DomainProject, dep.Consumer)

	oldProviderRules, err := TransferToMicroServiceDependency(ctx, conKey)
	if err != nil {
		log.Errorf(err, "update dependency rule failed, get consumer[%s/%s/%s/%s]'s dependency rule failed",
			dep.Consumer.Environment, dep.Consumer.AppId, dep.Consumer.ServiceName, dep.Consumer.Version)
		return
	}

	deleteDependencyRuleList = make([]*pb.MicroServiceKey, 0, len(oldProviderRules.Dependency))
	createDependencyRuleList = make([]*pb.MicroServiceKey, 0, len(dep.ProvidersRule))
	existDependencyRuleList = make([]*pb.MicroServiceKey, 0, len(oldProviderRules.Dependency))
	for _, tmpProviderRule := range dep.ProvidersRule {
		if ok, _ := ContainServiceDependency(oldProviderRules.Dependency, tmpProviderRule); ok {
			continue
		}

		if tmpProviderRule.ServiceName == "*" {
			createDependencyRuleList = append([]*pb.MicroServiceKey{}, tmpProviderRule)
			deleteDependencyRuleList = oldProviderRules.Dependency
			break
		}

		createDependencyRuleList = append(createDependencyRuleList, tmpProviderRule)
		old := IsNeedUpdate(oldProviderRules.Dependency, tmpProviderRule)
		if old != nil {
			deleteDependencyRuleList = append(deleteDependencyRuleList, old)
		}
	}
	for _, oldProviderRule := range oldProviderRules.Dependency {
		if oldProviderRule.ServiceName == "*" {
			createDependencyRuleList = nil
			deleteDependencyRuleList = nil
			return
		}
		if ok, _ := ContainServiceDependency(deleteDependencyRuleList, oldProviderRule); !ok {
			existDependencyRuleList = append(existDependencyRuleList, oldProviderRule)
		}
	}

	dep.ProvidersRule = append(createDependencyRuleList, existDependencyRuleList...)
	return
}

func parseOverrideRules(ctx context.Context, dep *Dependency) (createDependencyRuleList, existDependencyRuleList, deleteDependencyRuleList []*pb.MicroServiceKey) {
	conKey := apt.GenerateConsumerDependencyRuleKey(dep.DomainProject, dep.Consumer)

	oldProviderRules, err := TransferToMicroServiceDependency(ctx, conKey)
	if err != nil {
		log.Errorf(err, "override dependency rule failed, get consumer[%s/%s/%s/%s]'s dependency rule failed",
			dep.Consumer.Environment, dep.Consumer.AppId, dep.Consumer.ServiceName, dep.Consumer.Version)
		return
	}

	deleteDependencyRuleList = make([]*pb.MicroServiceKey, 0, len(oldProviderRules.Dependency))
	createDependencyRuleList = make([]*pb.MicroServiceKey, 0, len(dep.ProvidersRule))
	existDependencyRuleList = make([]*pb.MicroServiceKey, 0, len(oldProviderRules.Dependency))
	for _, oldProviderRule := range oldProviderRules.Dependency {
		if ok, _ := ContainServiceDependency(dep.ProvidersRule, oldProviderRule); !ok {
			deleteDependencyRuleList = append(deleteDependencyRuleList, oldProviderRule)
		} else {
			existDependencyRuleList = append(existDependencyRuleList, oldProviderRule)
		}
	}
	for _, tmpProviderRule := range dep.ProvidersRule {
		if ok, _ := ContainServiceDependency(existDependencyRuleList, tmpProviderRule); !ok {
			createDependencyRuleList = append(createDependencyRuleList, tmpProviderRule)
		}
	}
	return
}

func syncDependencyRule(ctx context.Context, dep *Dependency, filter func(context.Context, *Dependency) (_, _, _ []*pb.MicroServiceKey)) error {
	//更新consumer的providers的值,consumer的版本是确定的
	consumerFlag := strings.Join([]string{dep.Consumer.Environment, dep.Consumer.AppId, dep.Consumer.ServiceName, dep.Consumer.Version}, "/")

	createDependencyRuleList, existDependencyRuleList, deleteDependencyRuleList := filter(ctx, dep)
	if len(createDependencyRuleList) == 0 && len(existDependencyRuleList) == 0 && len(deleteDependencyRuleList) == 0 {
		return nil
	}

	if len(deleteDependencyRuleList) != 0 {
		log.Infof("delete consumer[%s]'s dependency rule %v", consumerFlag, deleteDependencyRuleList)
		dep.DeleteDependencyRuleList = deleteDependencyRuleList
	}

	if len(createDependencyRuleList) != 0 {
		log.Infof("create consumer[%s]'s dependency rule %v", consumerFlag, createDependencyRuleList)
		dep.CreateDependencyRuleList = createDependencyRuleList
	}

	return dep.Commit(ctx)
}

func AddDependencyRule(ctx context.Context, dep *Dependency) error {
	return syncDependencyRule(ctx, dep, parseAddOrUpdateRules)
}

func CreateDependencyRule(ctx context.Context, dep *Dependency) error {
	return syncDependencyRule(ctx, dep, parseOverrideRules)
}

func IsNeedUpdate(services []*pb.MicroServiceKey, service *pb.MicroServiceKey) *pb.MicroServiceKey {
	for _, tmp := range services {
		if DiffServiceVersion(tmp, service) {
			return tmp
		}
	}
	return nil
}

func ContainServiceDependency(services []*pb.MicroServiceKey, service *pb.MicroServiceKey) (bool, error) {
	if services == nil || service == nil {
		return false, errors.New("invalid params input")
	}
	for _, value := range services {
		rst := EqualServiceDependency(service, value)
		if rst {
			return true, nil
		}
	}
	return false, nil
}

func BadParamsResponse(detailErr string) *pb.CreateDependenciesResponse {
	log.Errorf(nil, "request params is invalid. %s", detailErr)
	if len(detailErr) == 0 {
		detailErr = "Request params is invalid."
	}
	return &pb.CreateDependenciesResponse{
		Response: pb.CreateResponse(scerr.ErrInvalidParams, detailErr),
	}
}

func ParamsChecker(consumerInfo *pb.MicroServiceKey, providersInfo []*pb.MicroServiceKey) *pb.CreateDependenciesResponse {
	flag := make(map[string]bool, len(providersInfo))
	for _, providerInfo := range providersInfo {
		//存在带*的情况，后面的数据就不校验了
		if providerInfo.ServiceName == "*" {
			break
		}
		if len(providerInfo.AppId) == 0 {
			providerInfo.AppId = consumerInfo.AppId
		}

		version := providerInfo.Version
		if len(version) == 0 {
			return BadParamsResponse("Required provider version")
		}

		providerInfo.Version = ""
		if _, ok := flag[toString(providerInfo)]; ok {
			return BadParamsResponse("Invalid request body for provider info.Duplicate provider or (serviceName and appId is same).")
		}
		flag[toString(providerInfo)] = true
		providerInfo.Version = version
	}
	return nil
}

func DeleteDependencyForDeleteService(domainProject string, serviceID string, service *pb.MicroServiceKey) (client.PluginOp, error) {
	key := apt.GenerateConsumerDependencyQueueKey(domainProject, serviceID, apt.DepsQueueUUID)
	conDep := new(pb.ConsumerDependency)
	conDep.Consumer = service
	conDep.Providers = []*pb.MicroServiceKey{}
	conDep.Override = true
	data, err := json.Marshal(conDep)
	if err != nil {
		return client.PluginOp{}, err
	}
	return client.OpPut(client.WithStrKey(key), client.WithValue(data)), nil
}

func removeProviderRuleOfConsumer(ctx context.Context, domainProject string, cache map[string]bool) ([]client.PluginOp, error) {
	key := apt.GenerateConsumerDependencyRuleKey(domainProject, nil) + apt.SPLIT
	resp, err := kv.Store().DependencyRule().Search(ctx,
		client.WithStrKey(key), client.WithPrefix())
	if err != nil {
		return nil, err
	}

	var ops []client.PluginOp
loop:
	for _, kv := range resp.Kvs {
		var left []*pb.MicroServiceKey
		all := kv.Value.(*pb.MicroServiceDependency).Dependency
		for _, key := range all {
			if key.ServiceName == "*" {
				continue loop
			}

			id := apt.GenerateProviderDependencyRuleKey(key.Tenant, key)
			exist, ok := cache[id]
			if !ok {
				_, exist, err = FindServiceIds(ctx, key.Version, key)
				if err != nil {
					return nil, fmt.Errorf("%v, find service %s/%s/%s/%s",
						err, key.Tenant, key.AppId, key.ServiceName, key.Version)
				}
				cache[id] = exist
			}

			if exist {
				left = append(left, key)
			}
		}

		if len(all) == len(left) {
			continue
		}

		if len(left) == 0 {
			ops = append(ops, client.OpDel(client.WithKey(kv.Key)))
		} else {
			val, err := json.Marshal(&pb.MicroServiceDependency{Dependency: left})
			if err != nil {
				return nil, fmt.Errorf("%v, marshal %v", err, left)
			}
			ops = append(ops, client.OpPut(client.WithKey(kv.Key), client.WithValue(val)))
		}
	}
	return ops, nil
}

func RemoveProviderRuleKeys(ctx context.Context, domainProject string, cache map[string]bool) ([]client.PluginOp, error) {
	key := apt.GenerateProviderDependencyRuleKey(domainProject, nil) + apt.SPLIT
	resp, err := kv.Store().DependencyRule().Search(ctx,
		client.WithStrKey(key), client.WithPrefix(), client.WithKeyOnly())
	if err != nil {
		return nil, err
	}

	var ops []client.PluginOp
	for _, kv := range resp.Kvs {
		id := util.BytesToStringWithNoCopy(kv.Key)
		exist, ok := cache[id]
		if !ok {
			_, key := apt.GetInfoFromDependencyRuleKV(kv.Key)
			if key == nil || key.ServiceName == "*" {
				continue
			}

			_, exist, err = FindServiceIds(ctx, key.Version, key)
			if err != nil {
				return nil, fmt.Errorf("find service %s/%s/%s/%s, %v",
					key.Tenant, key.AppId, key.ServiceName, key.Version, err)
			}
			cache[id] = exist
		}

		if !exist {
			ops = append(ops, client.OpDel(client.WithKey(kv.Key)))
		}
	}
	return ops, nil
}

func CleanUpDependencyRules(ctx context.Context, domainProject string) error {
	if len(domainProject) == 0 {
		return errors.New("required domainProject")
	}

	cache := make(map[string]bool)
	pOps, err := removeProviderRuleOfConsumer(ctx, domainProject, cache)
	if err != nil {
		return err
	}

	kOps, err := RemoveProviderRuleKeys(ctx, domainProject, cache)
	if err != nil {
		return err
	}

	ops := append(append([]client.PluginOp(nil), pOps...), kOps...)
	if len(ops) == 0 {
		return nil
	}

	return client.BatchCommit(ctx, ops)
}
