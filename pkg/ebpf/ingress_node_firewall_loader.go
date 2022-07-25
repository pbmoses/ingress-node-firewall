package nodefwloader

import (
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"k8s.io/klog"

	ingressnodefwiov1alpha1 "ingress-node-firewall/api/v1alpha1"
)

const (
	xdpDeny   = 1 // XDP_DROP value
	xdpAllow  = 2 // XDP_PASS value
	bpfFSPath = "/sys/fs/bpf"
)

// IngNodeFwController structure is the object hold controls for starting
// ingress node firewall resource
type IngNodeFwController struct {
	// eBPF objs to create/update eBPF maps
	objs bpfObjects
	// eBPF interfaces attachment objects
	links []link.Link
	// eBPF pingPath
	pinPath string
}

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS -type ruleType_st -type event_hdr_st -type ruleStatistics_st bpf ../../bpf/ingress_node_firewall_kernel.c -- -I ../../bpf/headers -I/usr/include/x86_64-linux-gnu/

// NewIngNodeFwController creates new IngressNodeFirewall controller object
func NewIngNodeFwController() (*IngNodeFwController, error) {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, err
	}

	pinDir := path.Join(bpfFSPath, "xdp_ingress_node_firewall_process")
	if err := os.MkdirAll(pinDir, os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to create pinDir %s: %s", pinDir, err)
	}
	// Load pre-compiled programs into the kernel.
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, &ebpf.CollectionOptions{Maps: ebpf.MapOptions{PinPath: pinDir}}); err != nil {
		return nil, fmt.Errorf("loading objects: pinDir:%s, err:%s", pinDir, err)
	}
	return &IngNodeFwController{
		objs:    objs,
		pinPath: pinDir,
	}, nil
}

// IngressNodeFwRulesLoader Add/Update/Delete ingress nod firewll rules to eBPF LPM MAP
func (infc *IngNodeFwController) IngressNodeFwRulesLoader(ingFireWallConfig ingressnodefwiov1alpha1.IngressNodeFirewallRules, isDelete bool) error {
	objs := infc.objs
	info, err := objs.bpfMaps.IngressNodeFirewallTableMap.Info()
	if err != nil {
		return fmt.Errorf("Cannot get map info: %v", err)
	}
	klog.Infof("Ingress node firewall map Info: %+v with FD %s", info, objs.bpfMaps.IngressNodeFirewallTableMap.String())

	if err := infc.makeIngressFwRulesMap(ingFireWallConfig, isDelete); err != nil {
		return fmt.Errorf("Failed to create map firewall rules: %v", err)
	}

	if err := infc.ingressNodeFwEvents(); err != nil {
		return fmt.Errorf("Failed to load perf stats: %v", err)
	}

	infc.ingressNodeFwStats()

	return nil
}

// makeIngressFwRulesMap convert IngressNodeFirewallRules into eBPF format which matched what the
// kerenl hook will be using.
func (infc *IngNodeFwController) makeIngressFwRulesMap(ingFirewallConfig ingressnodefwiov1alpha1.IngressNodeFirewallRules, isDelete bool) error {
	objs := infc.objs
	rules := bpfRulesValSt{}
	var key bpfBpfLpmIpKeySt

	// Parse firewall rules
	rules.NumRules = uint32(len(ingFirewallConfig.FirewallProtocolRules))
	for idx, rule := range ingFirewallConfig.FirewallProtocolRules {
		rules.Rules[idx].RuleId = rule.Order
		switch rule.Protocol {
		case ingressnodefwiov1alpha1.ProtocolTypeTCP:
			start, end, err := parseDstPorts(rule.ProtocolRule.Ports)
			if err != nil {
				return fmt.Errorf("Invalid Ports %s for protocol %v", rule.ProtocolRule.Ports, rule.Protocol)
			}
			rules.Rules[idx].DstPortStart = start
			rules.Rules[idx].DstPortEnd = end
			rules.Rules[idx].Protocol = syscall.IPPROTO_TCP
		case ingressnodefwiov1alpha1.ProtocolTypeUDP:
			start, end, err := parseDstPorts(rule.ProtocolRule.Ports)
			if err != nil {
				return fmt.Errorf("Invalid Ports %s for protocol %v", rule.ProtocolRule.Ports, rule.Protocol)
			}
			rules.Rules[idx].DstPortStart = start
			rules.Rules[idx].DstPortEnd = end
			rules.Rules[idx].Protocol = syscall.IPPROTO_UDP
		case ingressnodefwiov1alpha1.ProtocolTypeSCTP:
			start, end, err := parseDstPorts(rule.ProtocolRule.Ports)
			if err != nil {
				return fmt.Errorf("Invalid Ports %s for protocol %v", rule.ProtocolRule.Ports, rule.Protocol)
			}
			rules.Rules[idx].DstPortStart = start
			rules.Rules[idx].DstPortEnd = end
			rules.Rules[idx].Protocol = syscall.IPPROTO_SCTP
		case ingressnodefwiov1alpha1.ProtocolTypeICMP:
			rules.Rules[idx].IcmpType = rule.ICMPRule.ICMPType
			rules.Rules[idx].IcmpCode = rule.ICMPRule.ICMPCode
			rules.Rules[idx].Protocol = syscall.IPPROTO_ICMP
		case ingressnodefwiov1alpha1.ProtocolTypeICMPv6:
			rules.Rules[idx].IcmpType = rule.ICMPRule.ICMPType
			rules.Rules[idx].IcmpCode = rule.ICMPRule.ICMPCode
			rules.Rules[idx].Protocol = syscall.IPPROTO_ICMPV6

		default:
			return fmt.Errorf("Failed invalid protocol %v", rule.Protocol)
		}
		switch rule.Action {
		case ingressnodefwiov1alpha1.IngressNodeFirewallAllow:
			rules.Rules[idx].Action = xdpAllow
		case ingressnodefwiov1alpha1.IngressNodeFirewallDeny:
			rules.Rules[idx].Action = xdpDeny
		default:
			return fmt.Errorf("Failed invalid action %v", rule.Action)
		}
	}

	// Parse CIDRs to construct map keys wih shared rules
	for _, cidr := range ingFirewallConfig.SourceCIDRs {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("Failed to parse SourceCIDRs: %v", err)
		}
		if ip.To4() != nil {
			copy(key.IpData[:], ip.To4())
		} else {
			copy(key.IpData[:], ip.To16())
		}
		pfLen, _ := ipNet.Mask.Size()
		key.PrefixLen = uint32(pfLen)
		// Handle Ingress firewall map operation
		if isDelete {
			log.Printf("Deleting ingress firewall rules for key %v", key)
			if err := objs.bpfMaps.IngressNodeFirewallTableMap.Delete(key); err != nil {
				return fmt.Errorf("Failed Deleting ingress firewall rules: %v", err)
			}
		} else {
			log.Printf("Creating ingress firewall rules for key %v", key)
			if err := objs.bpfMaps.IngressNodeFirewallTableMap.Update(key, rules, ebpf.UpdateAny); err != nil {
				return fmt.Errorf("Failed Adding/Updating ingress firewall rules: %v", err)
			}
		}
	}
	return nil
}

// IngressNodeFwAttach attach eBPF program to list interfaces and pin them to different pinDir
func (infc *IngNodeFwController) IngressNodeFwAttach(ifacesName []string, isDelete bool) error {
	objs := infc.objs
	for _, ifaceName := range ifacesName {
		// Look up the network interface by name.
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return fmt.Errorf("lookup network iface %q: %s", ifaceName, err)
		}
		if !isDelete {
			// Attach the program.
			l, err := link.AttachXDP(link.XDPOptions{
				Program:   objs.IngresNodeFirewallProcess,
				Interface: iface.Index,
			})
			if err != nil {
				return fmt.Errorf("could not attach XDP program: %s", err)
			}
			lPinDir := path.Join(infc.pinPath, ifaceName+"_link")
			if err := l.Pin(lPinDir); err != nil {
				return fmt.Errorf("failed to pin link to pinDir %s: %s", lPinDir, err)
			}
			infc.links = append(infc.links, l)
			log.Printf("Attached IngressNode Firewall program to iface %q (index %d)", iface.Name, iface.Index)
		} else {
			log.Printf("Unattaching IngressNode Firewall program from iface %q (index %d)", iface.Name, iface.Index)
			infc.cleanup()
		}
	}
	return nil
}

// cleanup will delete all link objects for all interfaces and remove all the maps
func (infc *IngNodeFwController) cleanup() {
	for _, l := range infc.links {
		l := l
		l.Unpin()
		l.Close()
	}
	infc.objs.Close()
}

func parseDstPorts(ports string) (uint16, uint16, error) {
	if !strings.Contains(ports, "-") {
		port, err := strconv.ParseUint(ports, 10, 16)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid Port number %v", err)
		}
		return uint16(port), 0, nil
	}
	ps := strings.SplitN(ports, "-", 2)
	if len(ps) != 2 {
		return 0, 0, fmt.Errorf("invalid Ports range, Expected two integers seperated by hyphen but found  %q", ports)
	}
	startPort, err := strconv.ParseUint(ps[0], 10, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid Start DstPort number %s", err)
	}
	endPort, err := strconv.ParseUint(ps[1], 10, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid End DstPort number %s", err)
	}
	return uint16(startPort), uint16(endPort), nil
}