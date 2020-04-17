package bridge

import (
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"sync"

	"github.com/docker/docker/daemon/networkdriver"
	"github.com/docker/docker/daemon/networkdriver/ipallocator"
	"github.com/docker/docker/daemon/networkdriver/portallocator"
	"github.com/docker/docker/daemon/networkdriver/portmapper"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/iptables"
	"github.com/docker/docker/pkg/log"
	"github.com/docker/docker/pkg/networkfs/resolvconf"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/docker/libcontainer/netlink"
)

const (
	DefaultNetworkBridge     = "docker0"
	MaxAllocatedPortAttempts = 10
)

// Network interface represents the networking stack of a container
type networkInterface struct {
	IP           net.IP
	PortMappings []net.Addr // there are mappings to the host interfaces
}

type ifaces struct {
	c map[string]*networkInterface
	sync.Mutex
}

func (i *ifaces) Set(key string, n *networkInterface) {
	i.Lock()
	i.c[key] = n
	i.Unlock()
}

func (i *ifaces) Get(key string) *networkInterface {
	i.Lock()
	res := i.c[key]
	i.Unlock()
	return res
}

var (
	addrs = []string{
		// Here we don't follow the convention of using the 1st IP of the range for the gateway.
		// This is to use the same gateway IPs as the /24 ranges, which predate the /16 ranges.
		// In theory this shouldn't matter - in practice there's bound to be a few scripts relying
		// on the internal addressing or other stupid things like that.
		// They shouldn't, but hey, let's not break them unless we really have to.
		"172.17.42.1/16", // Don't use 172.16.0.0/16, it conflicts with EC2 DNS 172.16.0.23
		"10.0.42.1/16",   // Don't even try using the entire /8, that's too intrusive
		"10.1.42.1/16",
		"10.42.42.1/16",
		"172.16.42.1/24",
		"172.16.43.1/24",
		"172.16.44.1/24",
		"10.0.42.1/24",
		"10.0.43.1/24",
		"192.168.42.1/24",
		"192.168.43.1/24",
		"192.168.44.1/24",
	}

	bridgeIface   string
	bridgeNetwork *net.IPNet

	defaultBindingIP  = net.ParseIP("0.0.0.0")
	currentInterfaces = ifaces{c: make(map[string]*networkInterface)}
)

/*获取为 Docker 服务的网络设备地址。
创建指定 IP 地址的网桥。
配置网络 iptables 规则。
另外还为 eng 对象注册了多个 Handler ，如 allocate interface release interface
allocate _port 以及 link 等。*/
func InitDriver(job *engine.Job) engine.Status {
	var (
		network        *net.IPNet
		enableIPTables = job.GetenvBool("EnableIptables")
		icc            = job.GetenvBool("InterContainerCommunication")
		ipForward      = job.GetenvBool("EnableIpForward")
		bridgeIP       = job.Getenv("BridgeIP")
	)

	if defaultIP := job.Getenv("DefaultBindingIP"); defaultIP != "" {
		defaultBindingIP = net.ParseIP(defaultIP)
	}

	bridgeIface = job.Getenv("BridgeIface")
	usingDefaultBridge := false
	if bridgeIface == "" {
		usingDefaultBridge = true
		bridgeIface = DefaultNetworkBridge
	}

	//创建名为 dockerO 的网桥设备。
	addr, err := networkdriver.GetIfaceAddr(bridgeIface)
	if err != nil {
		// If we're not using the default bridge, fail without trying to create it
		//用户指定的网桥设备未找到
		if !usingDefaultBridge {
			job.Logf("bridge not found: %s", bridgeIface)
			return job.Error(err)
		}
		// If the iface is not found, try to create it
		//docke0未创建
		job.Logf("creating new bridge for %s", bridgeIface)

		//网桥网络地址的作用为: Docker Daemon 在创建 Docker 容器时，使用该网络地址为 Docker 容器分配 IP 地址

		if err := createBridge(bridgeIP); err != nil {
			return job.Error(err)
		}

		job.Logf("getting iface addr")
		addr, err = networkdriver.GetIfaceAddr(bridgeIface)
		if err != nil {
			return job.Error(err)
		}
		network = addr.(*net.IPNet)
	} else {
		//所在宿主机上 bridgeIface 的网桥设备存在时， Docker Daemon 仍然需要
		//验证用户在配置信息中是否为网桥设备指定了 IP 地址。
		network = addr.(*net.IPNet)
		// validate that the bridge ip matches the ip specified by BridgeIP
		if bridgeIP != "" {
			bip, _, err := net.ParseCIDR(bridgeIP)
			if err != nil {
				return job.Error(err)
			}
			if !network.IP.Equal(bip) {
				return job.Errorf("bridge ip (%s) does not match existing bridge configuration %s", network.IP, bip)
			}
		}
	}

	// Configure iptables for link support
	//创建完网桥之后， Docker Daemon 为未来的 Docker 容器以及宿主机配置 iptables 规则，
	//作用是:为 Docker 容器之间的 link 操作提供 iptables 防火墙支持
	if enableIPTables {
		if err := setupIPTables(addr, icc); err != nil {
			return job.Error(err)
		}
	}

	//启用系统数据包转发功能

	//Linux 系统上，网络设备之间的数据包转发功能是默认禁止的。数据包转发，即当宿
	//主机存在多块网络设备时，如果其中一块网络设备接收到数据包，则无条件将其转发给另外
	//的网络设备。通过修改 /proc/sys/net/ ipv4/ip_ forward 的值，将其置为 1，则可以保证系统内数
	//据包可以实现转发功能，
	if ipForward {
		// Enable IPv4 forwarding
		if err := ioutil.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte{'1', '\n'}, 0644); err != nil {
			job.Logf("WARNING: unable to enable IPv4 forwarding: %s\n", err)
		}
	}

	//Docker 在网桥设备上创建一条名为 DOCKER 的链，该链的作用是在创建 Docker 容器 时实现容器与宿主机的端口映射
	// We can always try removing the iptables
	if err := iptables.RemoveExistingChain("DOCKER"); err != nil {
		return job.Error(err)
	}

	if enableIPTables {
		chain, err := iptables.NewChain("DOCKER", bridgeIface)
		if err != nil {
			return job.Error(err)
		}
		portmapper.SetIptablesChain(chain)
	}

	bridgeNetwork = network

	// https://github.com/docker/docker/issues/2768
	job.Eng.Hack_SetGlobalVar("httpapi.bridgeIP", bridgeNetwork.IP)

	for name, f := range map[string]engine.Handler{
		"allocate_interface": Allocate,       //: Docker 容器分配专属网络接口，分配容器网段的 IP 地址;
		"release_interface":  Release,        //:释放 Docker 容器占用的网络接口资源;
		"allocate_port":      AllocatePort,   //: Docker 容器分配一个端口;
		"link":               LinkContainers, //实现 Docker 容器间的连接操作。
	} {
		if err := job.Eng.Register(name, f); err != nil {
			return job.Error(err)
		}
	}
	return engine.StatusOK
}

//配置 iptables 规则，
func setupIPTables(addr net.Addr, icc bool) error {
	// Enable NAT
	//使用 iptables 工具开启新建网桥的 NAT 功能
	natArgs := []string{"POSTROUTING", "-t", "nat", "-s", addr.String(), "!", "-o", bridgeIface, "-j", "MASQUERADE"}

	if !iptables.Exists(natArgs...) {
		if output, err := iptables.Raw(append([]string{"-I"}, natArgs...)...); err != nil {
			return fmt.Errorf("Unable to enable network bridge NAT: %s", err)
		} else if len(output) != 0 {
			return fmt.Errorf("Error iptables postrouting: %s", output)
		}
	}
	//从 dockerO 出来的数据包，如果需 要继续发往 dockerO ，则说明是 Docker 容器间的通信数据包。
	var (
		args       = []string{"FORWARD", "-i", bridgeIface, "-o", bridgeIface, "-j"}
		acceptArgs = append(args, "ACCEPT")
		dropArgs   = append(args, "DROP")
	)

	if !icc {
		iptables.Raw(append([]string{"-D"}, acceptArgs...)...)

		if !iptables.Exists(dropArgs...) {
			log.Debugf("Disable inter-container communication")
			if output, err := iptables.Raw(append([]string{"-I"}, dropArgs...)...); err != nil {
				return fmt.Errorf("Unable to prevent intercontainer communication: %s", err)
			} else if len(output) != 0 {
				return fmt.Errorf("Error disabling intercontainer communication: %s", output)
			}
		}
	} else {
		iptables.Raw(append([]string{"-D"}, dropArgs...)...)

		if !iptables.Exists(acceptArgs...) {
			log.Debugf("Enable inter-container communication")
			if output, err := iptables.Raw(append([]string{"-I"}, acceptArgs...)...); err != nil {
				return fmt.Errorf("Unable to allow intercontainer communication: %s", err)
			} else if len(output) != 0 {
				return fmt.Errorf("Error enabling intercontainer communication: %s", output)
			}
		}
	}

	// Accept all non-intercontainer outgoing packets
	//允许所有从 dockerO 发出且不是继续发向 dockerO 的数据包
	outgoingArgs := []string{"FORWARD", "-i", bridgeIface, "!", "-o", bridgeIface, "-j", "ACCEPT"}
	if !iptables.Exists(outgoingArgs...) {
		if output, err := iptables.Raw(append([]string{"-I"}, outgoingArgs...)...); err != nil {
			return fmt.Errorf("Unable to allow outgoing packets: %s", err)
		} else if len(output) != 0 {
			return fmt.Errorf("Error iptables allow outgoing: %s", output)
		}
	}

	// Accept incoming packets for existing connections
	//对于发往 dockerO ，并且属于已经建立的连接的数据包， Docker 无条件接受这些连接上的数据包，
	existingArgs := []string{"FORWARD", "-o", bridgeIface, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}

	if !iptables.Exists(existingArgs...) {
		if output, err := iptables.Raw(append([]string{"-I"}, existingArgs...)...); err != nil {
			return fmt.Errorf("Unable to allow incoming packets: %s", err)
		} else if len(output) != 0 {
			return fmt.Errorf("Error iptables allow incoming: %s", output)
		}
	}
	return nil
}

// CreateBridgeIface creates a network bridge interface on the host system with the name `ifaceName`,
// and attempts to configure it with an address which doesn't conflict with any other interface on the host.
// If it can't find an address which doesn't conflict, it will return an error.
func createBridge(bridgeIP string) error {
	nameservers := []string{}
	resolvConf, _ := resolvconf.Get()
	// we don't check for an error here, because we don't really care
	// if we can't read /etc/resolv.conf. So instead we skip the append
	// if resolvConf is nil. It either doesn't exist, or we can't read it
	// for some reason.
	if resolvConf != nil {
		nameservers = append(nameservers, resolvconf.GetNameserversAsCIDR(resolvConf)...)
	}

	var ifaceAddr string
	if len(bridgeIP) != 0 {
		_, _, err := net.ParseCIDR(bridgeIP)
		if err != nil {
			return err
		}
		ifaceAddr = bridgeIP
	} else {
		//从预定ip中选择可用地址
		for _, addr := range addrs {
			_, dockerNetwork, err := net.ParseCIDR(addr)
			if err != nil {
				return err
			}
			if err := networkdriver.CheckNameserverOverlaps(nameservers, dockerNetwork); err == nil {
				if err := networkdriver.CheckRouteOverlaps(dockerNetwork); err == nil {
					ifaceAddr = addr
					break
				} else {
					log.Debugf("%s %s", addr, err)
				}
			}
		}
	}

	if ifaceAddr == "" {
		return fmt.Errorf("Could not find a free IP address range for interface '%s'. Please configure its address manually and run 'docker -b %s'", bridgeIface, bridgeIface)
	}
	log.Debugf("Creating bridge %s with network %s", bridgeIface, ifaceAddr)

	//创建网桥
	if err := createBridgeIface(bridgeIface); err != nil {
		return err
	}

	iface, err := net.InterfaceByName(bridgeIface)
	if err != nil {
		return err
	}

	ipAddr, ipNet, err := net.ParseCIDR(ifaceAddr)
	if err != nil {
		return err
	}

	//通过netlink 机制为一个网络接口设备绑定一个 IP 地址。
	if netlink.NetworkLinkAddIp(iface, ipAddr, ipNet); err != nil {
		return fmt.Errorf("Unable to add private network: %s", err)
	}
	//启动网桥
	if err := netlink.NetworkLinkUp(iface); err != nil {
		return fmt.Errorf("Unable to start network bridge: %s", err)
	}
	return nil
}

func createBridgeIface(name string) error {
	kv, err := kernel.GetKernelVersion()
	// only set the bridge's mac address if the kernel version is > 3.3
	// before that it was not supported
	setBridgeMacAddr := err == nil && (kv.Kernel >= 3 && kv.Major >= 3)
	log.Debugf("setting bridge mac address = %v", setBridgeMacAddr)
	return netlink.CreateBridge(name, setBridgeMacAddr)
}

// Allocate a network interface
func Allocate(job *engine.Job) engine.Status {
	var (
		ip          *net.IP
		err         error
		id          = job.Args[0]
		requestedIP = net.ParseIP(job.Getenv("RequestedIP"))
	)

	if requestedIP != nil {
		ip, err = ipallocator.RequestIP(bridgeNetwork, &requestedIP)
	} else {
		ip, err = ipallocator.RequestIP(bridgeNetwork, nil)
	}
	if err != nil {
		return job.Error(err)
	}

	out := engine.Env{}
	out.Set("IP", ip.String())
	out.Set("Mask", bridgeNetwork.Mask.String())
	out.Set("Gateway", bridgeNetwork.IP.String())
	out.Set("Bridge", bridgeIface)

	size, _ := bridgeNetwork.Mask.Size()
	out.SetInt("IPPrefixLen", size)

	currentInterfaces.Set(id, &networkInterface{
		IP: *ip,
	})

	out.WriteTo(job.Stdout)

	return engine.StatusOK
}

// release an interface for a select ip
func Release(job *engine.Job) engine.Status {
	var (
		id                 = job.Args[0]
		containerInterface = currentInterfaces.Get(id)
	)

	if containerInterface == nil {
		return job.Errorf("No network information to release for %s", id)
	}

	for _, nat := range containerInterface.PortMappings {
		if err := portmapper.Unmap(nat); err != nil {
			log.Infof("Unable to unmap port %s: %s", nat, err)
		}
	}

	if err := ipallocator.ReleaseIP(bridgeNetwork, &containerInterface.IP); err != nil {
		log.Infof("Unable to release ip %s", err)
	}
	return engine.StatusOK
}

// Allocate an external port and map it to the interface
func AllocatePort(job *engine.Job) engine.Status {
	var (
		err error

		ip            = defaultBindingIP
		id            = job.Args[0]
		hostIP        = job.Getenv("HostIP")
		hostPort      = job.GetenvInt("HostPort")
		containerPort = job.GetenvInt("ContainerPort")
		proto         = job.Getenv("Proto")
		network       = currentInterfaces.Get(id)
	)

	if hostIP != "" {
		ip = net.ParseIP(hostIP)
	}

	// host ip, proto, and host port
	var container net.Addr
	switch proto {
	case "tcp":
		container = &net.TCPAddr{IP: network.IP, Port: containerPort}
	case "udp":
		container = &net.UDPAddr{IP: network.IP, Port: containerPort}
	default:
		return job.Errorf("unsupported address type %s", proto)
	}

	//
	// Try up to 10 times to get a port that's not already allocated.
	//
	// In the event of failure to bind, return the error that portmapper.Map
	// yields.
	//

	var host net.Addr
	for i := 0; i < MaxAllocatedPortAttempts; i++ {
		if host, err = portmapper.Map(container, ip, hostPort); err == nil {
			break
		}

		if allocerr, ok := err.(portallocator.ErrPortAlreadyAllocated); ok {
			// There is no point in immediately retrying to map an explicitly
			// chosen port.
			if hostPort != 0 {
				job.Logf("Failed to bind %s for container address %s: %s", allocerr.IPPort(), container.String(), allocerr.Error())
				break
			}

			// Automatically chosen 'free' port failed to bind: move on the next.
			job.Logf("Failed to bind %s for container address %s. Trying another port.", allocerr.IPPort(), container.String())
		} else {
			// some other error during mapping
			job.Logf("Received an unexpected error during port allocation: %s", err.Error())
			break
		}
	}

	if err != nil {
		return job.Error(err)
	}

	network.PortMappings = append(network.PortMappings, host)

	out := engine.Env{}
	switch netAddr := host.(type) {
	case *net.TCPAddr:
		out.Set("HostIP", netAddr.IP.String())
		out.SetInt("HostPort", netAddr.Port)
	case *net.UDPAddr:
		out.Set("HostIP", netAddr.IP.String())
		out.SetInt("HostPort", netAddr.Port)
	}
	if _, err := out.WriteTo(job.Stdout); err != nil {
		return job.Error(err)
	}

	return engine.StatusOK
}

func LinkContainers(job *engine.Job) engine.Status {
	var (
		action       = job.Args[0]
		childIP      = job.Getenv("ChildIP")
		parentIP     = job.Getenv("ParentIP")
		ignoreErrors = job.GetenvBool("IgnoreErrors")
		ports        = job.GetenvList("Ports")
	)
	split := func(p string) (string, string) {
		parts := strings.Split(p, "/")
		return parts[0], parts[1]
	}

	for _, p := range ports {
		port, proto := split(p)
		if output, err := iptables.Raw(action, "FORWARD",
			"-i", bridgeIface, "-o", bridgeIface,
			"-p", proto,
			"-s", parentIP,
			"--dport", port,
			"-d", childIP,
			"-j", "ACCEPT"); !ignoreErrors && err != nil {
			return job.Error(err)
		} else if len(output) != 0 {
			return job.Errorf("Error toggle iptables forward: %s", output)
		}

		if output, err := iptables.Raw(action, "FORWARD",
			"-i", bridgeIface, "-o", bridgeIface,
			"-p", proto,
			"-s", childIP,
			"--sport", port,
			"-d", parentIP,
			"-j", "ACCEPT"); !ignoreErrors && err != nil {
			return job.Error(err)
		} else if len(output) != 0 {
			return job.Errorf("Error toggle iptables forward: %s", output)
		}
	}
	return engine.StatusOK
}
