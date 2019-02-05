package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-sockaddr/template"
)

const defaultInterval = "10s"

type service struct {
	ID        string
	Name      string
	Port      int
	IP        string
	Kind      string
	ProxyPort int
	Tags      []string
	Meta      map[string]string
	TTL       int
}

var defaultID = flag.String("id", "", "Default service ID")
var defaultIP = flag.String("ip", "127.0.0.1", "Default IP to register service for (can be go-sockaddr template)")
var defaultName = flag.String("name", "", "Default service name")
var defaultPort = flag.Int("port", 0, "Default service port")
var httpAddr = flag.String("http-addr", "http://127.0.0.1:8500", "Consul agent URI")
var defaultTags = flag.String("tags", "", "Append tags for all registered services")
var taskArnTag = flag.String("task-arn-tag", "", "Tag name for ECS task ARN label")
var defaultArn = flag.String("task-arn", "", "Default ECS task ARN (if task-arn-tag set and task metadata endpoint not available)")
var retryAttempts = flag.Int("retry-attempts", 2, "Max retry attempts to establish a connection with the consul agent. Use -1 for infinite retries")
var retryInterval = flag.Int("retry-interval", 2000, "Interval (in milliseconds) between retry-attempts.")

func main() {
	var err error

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s [options]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NFlag() == 0 {
		// flags are not mandatory, but when running without flags, output usage message just in case
		flag.Usage()
	}

	executionEnv := os.Getenv("AWS_EXECUTION_ENV")
	var arn *string
	var v2endpoint = "http://169.254.170.2/v2/metadata"
	if *taskArnTag != "" {
		if executionEnv == "AWS_ECS_FARGATE" {
			// use task metadata endpoint v2
			arn, err = getTaskArn(v2endpoint)
		} else if executionEnv == "AWS_ECS_EC2" {
			// v2 availability assumes that we are using awsvpc networking
			// see: https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-metadata-endpoint.html
			arn, err = getTaskArn(v2endpoint)
		} else {
			arn = defaultArn
		}
		if arn == nil {
			err = errors.New("Unknown error occurred trying to determine task ARN")
		}
	}
	assert(err)
	if arn == nil {
		arn = new(string)
	}

	if *defaultIP != "" {
		ipAddrs := expandAddrs("-ip", defaultIP)
		if len(ipAddrs) == 0 {
			assert(errors.New("-ip cannot resolve to empty"))
		}
		if len(ipAddrs) > 1 {
			assert(errors.New("-ip cannot resolve to multiple addresses"))
		}
		if !isIPAddr(ipAddrs[0]) {
			assert(errors.New("-ip must resolve to an ip address"))
		}
		x := ipAddrs[0].(*net.IPAddr).String()
		log.Println("Default service IP", x)
		defaultIP = &x
	}

	var ports = declaredServicePorts()
	var services []*service
	log.Println("Declared service ports: ", ports)
	for i := range ports {
		port := ports[i]
		metadata, _ := serviceMetaData(strconv.Itoa(port))

		var tags = combineTags(mapDefault(metadata, "tags", ""), *defaultTags)
		var name = mapDefault(metadata, "name", *defaultName+"-"+strconv.Itoa(port))
		var id = mapDefault(metadata, "id", *defaultID)
		var portIP = mapDefault(metadata, "ip", *defaultIP)
		delete(metadata, "tags")
		delete(metadata, "name")
		delete(metadata, "id")
		delete(metadata, "ip")

		// SERVICE_x_IP can (and usually will) contain a go_sockaddr so we need to resolve it
		var expandedAddrs = expandAddrs(fmt.Sprintf("Port %v", port), &portIP)
		if len(expandedAddrs) == 0 {
			log.Printf("Port %v: skipping registration because SERVICE_%v_IP resolves to empty", port, port)
			continue
		}
		if len(expandedAddrs) > 1 {
			log.Printf("Port %v: skipping registration because SERVICE_%v_IP resolves to multiple IP addresses", port, port)
			continue
		}
		if !isIPAddr(expandedAddrs[0]) {
			log.Printf("Port %v: skipping registration because SERVICE_%v_IP does not resolve to an IP address", port, port)
			continue
		}
		var expandedIP = expandedAddrs[0].(*net.IPAddr).String()

		if name == "" {
			log.Printf("Port %v: skipping registration of SERVICE_%v_NAME env var is not set and no -name default provided", port, port)
			continue
		}
		if id == "" {
			// TODO: ideally we would be setting the service id to the container id from the task metadata
			var src = rand.NewSource(time.Now().UnixNano())
			id = strconv.FormatInt(src.Int63(), 16)
			log.Printf("Port %v: generating random service id %s because SERVICE_%v_ID env var is not set and no -id default provided", port, id, port)
		}
		if *arn != "" {
			tags = append(tags, *taskArnTag+"="+*arn)
		}

		service := &service{
			Name: name,
			ID:   id,
			IP:   expandedIP,
			Port: port,
			Tags: tags,
			Meta: metadata,
		}

		log.Printf("Port %v: service name %s, ip %s, id %s, tags %v \n", service.Port, service.Name, service.IP, service.ID, service.Tags)
		services = append(services, service)
	}

	client, err := newClient()
	assert(err)

	attempt := 0
	for *retryAttempts == -1 || attempt <= *retryAttempts {
		log.Printf("Connecting to consul agent (%v/%v) at %s", attempt, *retryAttempts, *httpAddr)
		err = ping(client)
		if err == nil {
			break
		}

		if attempt == *retryAttempts {
			assert(err)
		}

		time.Sleep(time.Duration(*retryInterval) * time.Millisecond)
		attempt++
	}

	for i := range services {
		service := services[i]
		reg := buildReg(service)
		err := client.Agent().ServiceRegister(reg)
		assert(err)
	}
}

func buildReg(service *service) *consulapi.AgentServiceRegistration {
	return &consulapi.AgentServiceRegistration{
		ID:      service.ID,
		Name:    service.Name,
		Port:    service.Port,
		Address: service.IP,
		Kind:    consulapi.ServiceKindTypical,
		Check:   buildCheck(service),
		Meta:    service.Meta,
	}
}

// ping will try to connect to consul by attempting to retrieve the current leader.
func ping(client *consulapi.Client) error {
	status := client.Status()
	leader, err := status.Leader()
	if err != nil {
		return err
	}
	log.Println("consul: current leader ", leader)

	return nil
}

func buildCheck(service *service) *consulapi.AgentServiceCheck {
	check := new(consulapi.AgentServiceCheck)
	if status := service.Meta["check_initial_status"]; status != "" {
		check.Status = status
	}
	if path := service.Meta["check_http"]; path != "" {
		check.HTTP = fmt.Sprintf("http://%s:%d%s", service.IP, service.Port, path)
		if timeout := service.Meta["check_timeout"]; timeout != "" {
			check.Timeout = timeout
		}
	} else if path := service.Meta["check_https"]; path != "" {
		check.HTTP = fmt.Sprintf("https://%s:%d%s", service.IP, service.Port, path)
		if timeout := service.Meta["check_timeout"]; timeout != "" {
			check.Timeout = timeout
		}
	} else if script := service.Meta["check_script"]; script != "" {
		check.Args = strings.Split(interpolateService(script, service), " ")
	} else if ttl := service.Meta["check_ttl"]; ttl != "" {
		check.TTL = ttl
	} else if tcp := service.Meta["check_tcp"]; tcp != "" {
		check.TCP = fmt.Sprintf("%s:%d", service.IP, service.Port)
		if timeout := service.Meta["check_timeout"]; timeout != "" {
			check.Timeout = timeout
		}
	} else {
		return nil
	}
	if len(check.Args) > 0 || check.HTTP != "" || check.TCP != "" {
		if interval := service.Meta["check_interval"]; interval != "" {
			check.Interval = interval
		} else {
			check.Interval = defaultInterval
		}
	}
	if deregisterAfter := service.Meta["check_deregister_after"]; deregisterAfter != "" {
		check.DeregisterCriticalServiceAfter = deregisterAfter
	}
	return check
}

func interpolateService(script string, service *service) string {
	withIP := strings.Replace(script, "$SERVICE_IP", service.IP, -1)
	withPort := strings.Replace(withIP, "$SERVICE_PORT", strconv.Itoa(service.Port), -1)
	return withPort
}

func assert(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func expandAddrs(name string, s *string) []net.Addr {
	x, err := template.Parse(*s)
	if err != nil {
		assert(fmt.Errorf("%s error parsing %q: %s", name, *s, err))
	}
	var addrs []net.Addr
	for _, a := range strings.Fields(x) {
		switch {
		case strings.HasPrefix(a, "unix://"):
			assert(fmt.Errorf("%s %s not supported", name, a))
		default:
			// net.ParseIP does not like '[::]'
			ip := net.ParseIP(a)
			if a == "[::]" {
				ip = net.ParseIP("::")
			}
			if ip == nil {
				assert(fmt.Errorf("%s invalid ip address: %s", name, a))
			}
			addrs = append(addrs, &net.IPAddr{IP: ip})
		}
	}

	return addrs
}

func isIPAddr(a net.Addr) bool {
	_, ok := a.(*net.IPAddr)
	return ok
}

func serviceMetaData(port string) (map[string]string, map[string]bool) {
	meta := os.Environ()
	metadata := make(map[string]string)
	metadataFromPort := make(map[string]bool)
	for _, kv := range meta {
		kvp := strings.SplitN(kv, "=", 2)
		if strings.HasPrefix(kvp[0], "SERVICE_") && len(kvp) > 1 {
			key := strings.ToLower(strings.TrimPrefix(kvp[0], "SERVICE_"))
			if metadataFromPort[key] {
				continue
			}
			portkey := strings.SplitN(key, "_", 2)
			_, err := strconv.Atoi(portkey[0])
			if err == nil && len(portkey) > 1 {
				if portkey[0] != port {
					continue
				}
				metadata[portkey[1]] = kvp[1]
				metadataFromPort[portkey[1]] = true
			} else {
				metadata[key] = kvp[1]
			}
		}
	}
	return metadata, metadataFromPort
}

func declaredServicePorts() []int {
	meta := os.Environ()
	var ports []int
	for _, kv := range meta {
		kvp := strings.SplitN(kv, "=", 2)
		if strings.HasPrefix(kvp[0], "SERVICE_") && len(kvp) > 1 {
			key := strings.ToLower(strings.TrimPrefix(kvp[0], "SERVICE_"))
			portkey := strings.SplitN(key, "_", 2)
			port, err := strconv.Atoi(portkey[0])
			if err == nil {
				ports = append(ports, port)
			}
		}
	}
	return ports
}

func mapDefault(m map[string]string, key, defaultVal string) string {
	v, ok := m[key]
	if !ok || v == "" {
		return defaultVal
	}
	return v
}

// Golang regexp module does not support /(?!\\),/ syntax for spliting by not escaped comma
// Then this function is reproducing it
func recParseEscapedComma(str string) []string {
	if len(str) == 0 {
		return []string{}
	} else if str[0] == ',' {
		return recParseEscapedComma(str[1:])
	}

	offset := 0
	for len(str[offset:]) > 0 {
		index := strings.Index(str[offset:], ",")

		if index == -1 {
			break
		} else if str[offset+index-1:offset+index] != "\\" {
			return append(recParseEscapedComma(str[offset+index+1:]), str[:offset+index])
		}

		str = str[:offset+index-1] + str[offset+index:]
		offset += index
	}

	return []string{str}
}

func combineTags(tagParts ...string) []string {
	tags := make([]string, 0)
	for _, element := range tagParts {
		tags = append(tags, recParseEscapedComma(element)...)
	}
	return tags
}

func newClient() (*consulapi.Client, error) {
	uri, err := url.Parse(*httpAddr)
	if err != nil {
		return nil, err
	}
	config := consulapi.DefaultConfig()
	switch scheme := uri.Scheme; scheme {
	case "http":
	// do nothing
	case "https":
		tlsConfigDesc := &consulapi.TLSConfig{
			Address:            uri.Host,
			CAFile:             os.Getenv("CONSUL_CACERT"),
			CertFile:           os.Getenv("CONSUL_TLSCERT"),
			KeyFile:            os.Getenv("CONSUL_TLSKEY"),
			InsecureSkipVerify: false,
		}
		tlsConfig, err := consulapi.SetupTLSConfig(tlsConfigDesc)
		if err != nil {
			return nil, err
		}
		config.Scheme = "https"
		transport := cleanhttp.DefaultPooledTransport()
		transport.TLSClientConfig = tlsConfig
		config.HttpClient.Transport = transport
	}
	config.Address = uri.Host
	client, err := consulapi.NewClient(config)
	return client, err
}

func getTaskArn(endpoint string) (*string, error) {

	response, err := http.DefaultClient.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	var jsonResult []byte
	if response.StatusCode == http.StatusOK {
		jsonResult, err = ioutil.ReadAll(response.Body)
	} else {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var taskMetadata map[string]interface{}
	json.Unmarshal(jsonResult, &taskMetadata)

	var taskARN = taskMetadata["TaskARN"].(string)

	return &taskARN, nil
}
