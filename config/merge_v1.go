package config

import (
	"fmt"
	"path"

	"github.com/sirupsen/logrus"
	yaml "github.com/cloudfoundry-incubator/candiedyaml"
	"github.com/hyperhq/libcompose/utils"
)

// MergeServicesV1 merges a v1 compose file into an existing set of service configs
func MergeServicesV1(existingServices *ServiceConfigs, environmentLookup EnvironmentLookup, resourceLookup ResourceLookup, file string, bytes []byte) (map[string]*ServiceConfigV1, error) {
	datas := make(RawServiceMap)
	if err := yaml.Unmarshal(bytes, &datas); err != nil {
		return nil, err
	}

	if err := Interpolate(environmentLookup, &datas); err != nil {
		return nil, err
	}

	if err := validate(datas, "v1"); err != nil {
		return nil, err
	}

	for name, data := range datas {
		data, err := parseV1(resourceLookup, environmentLookup, file, data, datas)
		if err != nil {
			logrus.Errorf("Failed to parse service %s: %v", name, err)
			return nil, err
		}

		if serviceConfig, ok := existingServices.Get(name); ok {
			var rawExistingService RawService
			if err := utils.Convert(serviceConfig, &rawExistingService); err != nil {
				return nil, err
			}

			data = mergeConfig(rawExistingService, data)
		}

		datas[name] = data
	}

	for name, data := range datas {
		err := validateServiceConstraints(data, name)
		if err != nil {
			return nil, err
		}
	}

	serviceConfigs := make(map[string]*ServiceConfigV1)
	if err := utils.Convert(datas, &serviceConfigs); err != nil {
		return nil, err
	}

	return serviceConfigs, nil
}

func parseV1(resourceLookup ResourceLookup, environmentLookup EnvironmentLookup, inFile string, serviceData RawService, datas RawServiceMap) (RawService, error) {
	serviceData, err := readEnvFile(resourceLookup, inFile, serviceData)
	if err != nil {
		return nil, err
	}

	//serviceData = resolveContextV1(inFile, serviceData)

	value, ok := serviceData["extends"]
	if !ok {
		return serviceData, nil
	}

	mapValue, ok := value.(map[interface{}]interface{})
	if !ok {
		return serviceData, nil
	}

	if resourceLookup == nil {
		return nil, fmt.Errorf("Can not use extends in file %s no mechanism provided to files", inFile)
	}

	file := asString(mapValue["file"])
	service := asString(mapValue["service"])

	if service == "" {
		return serviceData, nil
	}

	var baseService RawService

	if file == "" {
		if serviceData, ok := datas[service]; ok {
			baseService, err = parseV1(resourceLookup, environmentLookup, inFile, serviceData, datas)
		} else {
			return nil, fmt.Errorf("Failed to find service %s to extend", service)
		}
	} else {
		bytes, resolved, err := resourceLookup.Lookup(file, inFile)
		if err != nil {
			logrus.Errorf("Failed to lookup file %s: %v", file, err)
			return nil, err
		}

		var baseRawServices RawServiceMap
		if err := yaml.Unmarshal(bytes, &baseRawServices); err != nil {
			return nil, err
		}

		err = Interpolate(environmentLookup, &baseRawServices)
		if err != nil {
			return nil, err
		}

		if err := validate(baseRawServices, "v1"); err != nil {
			return nil, err
		}

		baseService, ok = baseRawServices[service]
		if !ok {
			return nil, fmt.Errorf("Failed to find service %s in file %s", service, file)
		}

		baseService, err = parseV1(resourceLookup, environmentLookup, resolved, baseService, baseRawServices)
	}

	if err != nil {
		return nil, err
	}

	baseService = clone(baseService)

	logrus.Debugf("Merging %#v, %#v", baseService, serviceData)

	for _, k := range noMerge {
		if _, ok := baseService[k]; ok {
			source := file
			if source == "" {
				source = inFile
			}
			return nil, fmt.Errorf("Cannot extend service '%s' in %s: services with '%s' cannot be extended", service, source, k)
		}
	}

	baseService = mergeConfig(baseService, serviceData)

	logrus.Debugf("Merged result %#v", baseService)

	return baseService, nil
}

func resolveContextV1(inFile string, serviceData RawService) RawService {
	context := asString(serviceData["build"])
	if context == "" {
		return serviceData
	}

	if IsValidRemote(context) {
		return serviceData
	}

	current := path.Dir(inFile)

	if context == "." {
		context = current
	} else {
		current = path.Join(current, context)
	}

	serviceData["build"] = current

	return serviceData
}
