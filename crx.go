package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	consulapi "github.com/hashicorp/consul/api"
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
var taskArnTag = flag.String("task-arn-tag", "traefik.tags", "Tag name for ECS task ARN label (only used for awsvpc)")
var arn = flag.String("task-arn", "", "ECS task ARN (get this from task metadata endpoint)")
var retryAttempts = flag.Int("retry-attempts", 2, "Max retry attempts to establish a connection with the consul agent. Use -1 for infinite retries")
var retryInterval = flag.Int("retry-interval", 2000, "Interval (in millisecond) between retry-attempts.")

func main() {

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s [options]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

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

		var tags = combineTags(mapDefault(metadata, "tags", ""))
		var name = mapDefault(metadata, "name", *defaultName+"-"+strconv.Itoa(port))
		var id = mapDefault(metadata, "id", *defaultID)
		var portIP = mapDefault(metadata, "ip", *defaultIP)
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

		service := new(service)
		service.Name = name
		service.ID = id
		service.IP = expandedIP
		service.Port = port
		service.Tags = tags
		service.Meta = metadata

		log.Printf("Port %v: service name %s, ip %s, id %s, tags %v \n", service.Port, service.Name, service.IP, service.ID, service.Tags)
		services = append(services, service)
	}

	config := consulapi.DefaultConfig()
	client, err := consulapi.NewClient(config)
	assert(err)

	attempt := 0
	for *retryAttempts == -1 || attempt <= *retryAttempts {
		log.Printf("Connecting to consul agent (%v/%v)", attempt, *retryAttempts)

		err = ping(client)
		if err == nil {
			break
		}

		if err != nil && attempt == *retryAttempts {
			assert(err)
		}

		time.Sleep(time.Duration(*retryInterval) * time.Millisecond)
		attempt++
	}

	for i := range services {
		service := services[i]
		reg := buildReg(service)

		res := client.Agent().ServiceRegister(reg)
		if res != nil {
			log.Fatal(res)
		}
	}
}

func buildReg(service *service) *consulapi.AgentServiceRegistration {
	registration := new(consulapi.AgentServiceRegistration)
	registration.ID = service.ID
	registration.Name = service.Name
	registration.Port = service.Port
	registration.Address = service.IP
	registration.Kind = consulapi.ServiceKindTypical
	registration.Check = buildCheck(service)
	registration.Meta = service.Meta
	return registration
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
