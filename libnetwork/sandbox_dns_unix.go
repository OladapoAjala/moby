//go:build !windows

package libnetwork

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/containerd/log"
	"github.com/docker/docker/libnetwork/etchosts"
	"github.com/docker/docker/libnetwork/resolvconf"
	"github.com/docker/docker/libnetwork/types"
)

const (
	defaultPrefix = "/var/lib/docker/network/files"
	dirPerm       = 0o755
	filePerm      = 0o644

	resolverIPSandbox = "127.0.0.11"
)

func (sb *Sandbox) startResolver(restore bool) {
	sb.resolverOnce.Do(func() {
		var err error
		// The embedded resolver is always started with proxyDNS set as true, even when the sandbox is only attached to
		// an internal network. This way, it's the driver responsibility to make sure `connect` syscall fails fast when
		// no external connectivity is available (eg. by not setting a default gateway).
		sb.resolver = NewResolver(resolverIPSandbox, true, sb)
		defer func() {
			if err != nil {
				sb.resolver = nil
			}
		}()

		// In the case of live restore container is already running with
		// right resolv.conf contents created before. Just update the
		// external DNS servers from the restored sandbox for embedded
		// server to use.
		if !restore {
			err = sb.rebuildDNS()
			if err != nil {
				log.G(context.TODO()).Errorf("Updating resolv.conf failed for container %s, %q", sb.ContainerID(), err)
				return
			}
		}
		sb.resolver.SetExtServers(sb.extDNS)

		if err = sb.osSbox.InvokeFunc(sb.resolver.SetupFunc(0)); err != nil {
			log.G(context.TODO()).Errorf("Resolver Setup function failed for container %s, %q", sb.ContainerID(), err)
			return
		}

		if err = sb.resolver.Start(); err != nil {
			log.G(context.TODO()).Errorf("Resolver Start failed for container %s, %q", sb.ContainerID(), err)
		}
	})
}

func (sb *Sandbox) setupResolutionFiles() error {
	if err := sb.buildHostsFile(); err != nil {
		return err
	}

	if err := sb.updateParentHosts(); err != nil {
		return err
	}

	return sb.setupDNS()
}

func (sb *Sandbox) buildHostsFile() error {
	if sb.config.hostsPath == "" {
		sb.config.hostsPath = defaultPrefix + "/" + sb.id + "/hosts"
	}

	dir, _ := filepath.Split(sb.config.hostsPath)
	if err := createBasePath(dir); err != nil {
		return err
	}

	// This is for the host mode networking
	if sb.config.useDefaultSandBox && len(sb.config.extraHosts) == 0 {
		// We are working under the assumption that the origin file option had been properly expressed by the upper layer
		// if not here we are going to error out
		if err := copyFile(sb.config.originHostsPath, sb.config.hostsPath); err != nil && !os.IsNotExist(err) {
			return types.InternalErrorf("could not copy source hosts file %s to %s: %v", sb.config.originHostsPath, sb.config.hostsPath, err)
		}
		return nil
	}

	extraContent := make([]etchosts.Record, 0, len(sb.config.extraHosts))
	for _, extraHost := range sb.config.extraHosts {
		extraContent = append(extraContent, etchosts.Record{Hosts: extraHost.name, IP: extraHost.IP})
	}

	return etchosts.Build(sb.config.hostsPath, "", sb.config.hostName, sb.config.domainName, extraContent)
}

func (sb *Sandbox) updateHostsFile(ifaceIPs []string) error {
	if len(ifaceIPs) == 0 {
		return nil
	}

	if sb.config.originHostsPath != "" {
		return nil
	}

	// User might have provided a FQDN in hostname or split it across hostname
	// and domainname.  We want the FQDN and the bare hostname.
	fqdn := sb.config.hostName
	if sb.config.domainName != "" {
		fqdn += "." + sb.config.domainName
	}
	hosts := fqdn

	if hostName, _, ok := strings.Cut(fqdn, "."); ok {
		hosts += " " + hostName
	}

	var extraContent []etchosts.Record
	for _, ip := range ifaceIPs {
		extraContent = append(extraContent, etchosts.Record{Hosts: hosts, IP: ip})
	}

	sb.addHostsEntries(extraContent)
	return nil
}

func (sb *Sandbox) addHostsEntries(recs []etchosts.Record) {
	if err := etchosts.Add(sb.config.hostsPath, recs); err != nil {
		log.G(context.TODO()).Warnf("Failed adding service host entries to the running container: %v", err)
	}
}

func (sb *Sandbox) deleteHostsEntries(recs []etchosts.Record) {
	if err := etchosts.Delete(sb.config.hostsPath, recs); err != nil {
		log.G(context.TODO()).Warnf("Failed deleting service host entries to the running container: %v", err)
	}
}

func (sb *Sandbox) updateParentHosts() error {
	var pSb *Sandbox

	for _, update := range sb.config.parentUpdates {
		// TODO(thaJeztah): was it intentional for this loop to re-use prior results of pSB? If not, we should make pSb local and always replace here.
		if s, _ := sb.controller.GetSandbox(update.cid); s != nil {
			pSb = s
		}
		if pSb == nil {
			continue
		}
		if err := etchosts.Update(pSb.config.hostsPath, update.ip, update.name); err != nil {
			return err
		}
	}

	return nil
}

func (sb *Sandbox) restorePath() {
	if sb.config.resolvConfPath == "" {
		sb.config.resolvConfPath = defaultPrefix + "/" + sb.id + "/resolv.conf"
	}
	sb.config.resolvConfHashFile = sb.config.resolvConfPath + ".hash"
	if sb.config.hostsPath == "" {
		sb.config.hostsPath = defaultPrefix + "/" + sb.id + "/hosts"
	}
}

func (sb *Sandbox) setExternalResolvers(content []byte, addrType int, checkLoopback bool) {
	servers := resolvconf.GetNameservers(content, addrType)
	for _, ip := range servers {
		hostLoopback := false
		if checkLoopback && isIPv4Loopback(ip) {
			hostLoopback = true
		}
		sb.extDNS = append(sb.extDNS, extDNSEntry{
			IPStr:        ip,
			HostLoopback: hostLoopback,
		})
	}
}

// isIPv4Loopback checks if the given IP address is an IPv4 loopback address.
// It's based on the logic in Go's net.IP.IsLoopback(), but only the IPv4 part:
// https://github.com/golang/go/blob/go1.16.6/src/net/ip.go#L120-L126
func isIPv4Loopback(ipAddress string) bool {
	if ip := net.ParseIP(ipAddress); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4[0] == 127
		}
	}
	return false
}

func (sb *Sandbox) setupDNS() error {
	if sb.config.resolvConfPath == "" {
		sb.config.resolvConfPath = defaultPrefix + "/" + sb.id + "/resolv.conf"
	}

	sb.config.resolvConfHashFile = sb.config.resolvConfPath + ".hash"

	dir, _ := filepath.Split(sb.config.resolvConfPath)
	if err := createBasePath(dir); err != nil {
		return err
	}

	// When the user specify a conainter in the host namespace and do no have any dns option specified
	// we just copy the host resolv.conf from the host itself
	if sb.config.useDefaultSandBox && len(sb.config.dnsList) == 0 && len(sb.config.dnsSearchList) == 0 && len(sb.config.dnsOptionsList) == 0 {
		// We are working under the assumption that the origin file option had been properly expressed by the upper layer
		// if not here we are going to error out
		if err := copyFile(sb.config.originResolvConfPath, sb.config.resolvConfPath); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("could not copy source resolv.conf file %s to %s: %v", sb.config.originResolvConfPath, sb.config.resolvConfPath, err)
			}
			log.G(context.TODO()).Infof("%s does not exist, we create an empty resolv.conf for container", sb.config.originResolvConfPath)
			if err := createFile(sb.config.resolvConfPath); err != nil {
				return err
			}
		}
		return nil
	}

	originResolvConfPath := sb.config.originResolvConfPath
	if originResolvConfPath == "" {
		// fallback if not specified
		originResolvConfPath = resolvconf.Path()
	}
	currRC, err := os.ReadFile(originResolvConfPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// No /etc/resolv.conf found: we'll use the default resolvers (Google's Public DNS).
		log.G(context.TODO()).WithField("path", originResolvConfPath).Infof("no resolv.conf found, falling back to defaults")
	}

	var newRC *resolvconf.File
	if len(sb.config.dnsList) > 0 || len(sb.config.dnsSearchList) > 0 || len(sb.config.dnsOptionsList) > 0 {
		var (
			dnsList        = sb.config.dnsList
			dnsSearchList  = sb.config.dnsSearchList
			dnsOptionsList = sb.config.dnsOptionsList
		)
		if len(sb.config.dnsList) == 0 {
			dnsList = resolvconf.GetNameservers(currRC, resolvconf.IP)
		}
		if len(sb.config.dnsSearchList) == 0 {
			dnsSearchList = resolvconf.GetSearchDomains(currRC)
		}
		if len(sb.config.dnsOptionsList) == 0 {
			dnsOptionsList = resolvconf.GetOptions(currRC)
		}
		newRC, err = resolvconf.Build(sb.config.resolvConfPath, dnsList, dnsSearchList, dnsOptionsList)
		if err != nil {
			return err
		}
		// After building the resolv.conf from the user config save the
		// external resolvers in the sandbox. Note that --dns 127.0.0.x
		// config refers to the loopback in the container namespace
		sb.setExternalResolvers(newRC.Content, resolvconf.IPv4, len(sb.config.dnsList) == 0)
	} else {
		// If the host resolv.conf file has 127.0.0.x container should
		// use the host resolver for queries. This is supported by the
		// docker embedded DNS server. Hence save the external resolvers
		// before filtering it out.
		sb.setExternalResolvers(currRC, resolvconf.IPv4, true)

		// Replace any localhost/127.* (at this point we have no info about ipv6, pass it as true)
		newRC, err = resolvconf.FilterResolvDNS(currRC, true)
		if err != nil {
			return err
		}
		// No contention on container resolv.conf file at sandbox creation
		err = os.WriteFile(sb.config.resolvConfPath, newRC.Content, filePerm)
		if err != nil {
			return types.InternalErrorf("failed to write unhaltered resolv.conf file content when setting up dns for sandbox %s: %v", sb.ID(), err)
		}
	}

	// Write hash
	err = os.WriteFile(sb.config.resolvConfHashFile, newRC.Hash, filePerm)
	if err != nil {
		return types.InternalErrorf("failed to write resolv.conf hash file when setting up dns for sandbox %s: %v", sb.ID(), err)
	}

	return nil
}

func (sb *Sandbox) updateDNS(ipv6Enabled bool) error {
	// This is for the host mode networking
	if sb.config.useDefaultSandBox {
		return nil
	}

	if len(sb.config.dnsList) > 0 || len(sb.config.dnsSearchList) > 0 || len(sb.config.dnsOptionsList) > 0 {
		return nil
	}

	var currHash []byte
	currRC, err := resolvconf.GetSpecific(sb.config.resolvConfPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else {
		currHash, err = os.ReadFile(sb.config.resolvConfHashFile)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	if len(currHash) > 0 && !bytes.Equal(currHash, currRC.Hash) {
		// Seems the user has changed the container resolv.conf since the last time
		// we checked so return without doing anything.
		// log.G(ctx).Infof("Skipping update of resolv.conf file with ipv6Enabled: %t because file was touched by user", ipv6Enabled)
		return nil
	}

	// replace any localhost/127.* and remove IPv6 nameservers if IPv6 disabled.
	newRC, err := resolvconf.FilterResolvDNS(currRC.Content, ipv6Enabled)
	if err != nil {
		return err
	}
	err = os.WriteFile(sb.config.resolvConfPath, newRC.Content, filePerm)
	if err != nil {
		return err
	}

	// write the new hash in a temp file and rename it to make the update atomic
	dir := path.Dir(sb.config.resolvConfPath)
	tmpHashFile, err := os.CreateTemp(dir, "hash")
	if err != nil {
		return err
	}
	if err = tmpHashFile.Chmod(filePerm); err != nil {
		tmpHashFile.Close()
		return err
	}
	_, err = tmpHashFile.Write(newRC.Hash)
	if err1 := tmpHashFile.Close(); err == nil {
		err = err1
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpHashFile.Name(), sb.config.resolvConfHashFile)
}

// Embedded DNS server has to be enabled for this sandbox. Rebuild the container's
// resolv.conf by doing the following
// - Add only the embedded server's IP to container's resolv.conf
// - If the embedded server needs any resolv.conf options add it to the current list
func (sb *Sandbox) rebuildDNS() error {
	currRC, err := os.ReadFile(sb.config.resolvConfPath)
	if err != nil {
		return err
	}

	// If the user config and embedded DNS server both have ndots option set,
	// remember the user's config so that unqualified names not in the docker
	// domain can be dropped.
	resOptions := sb.resolver.ResolverOptions()
	dnsOptionsList := resolvconf.GetOptions(currRC)

dnsOpt:
	for _, resOpt := range resOptions {
		if strings.Contains(resOpt, "ndots") {
			for _, option := range dnsOptionsList {
				if strings.Contains(option, "ndots") {
					parts := strings.Split(option, ":")
					if len(parts) != 2 {
						return fmt.Errorf("invalid ndots option %v", option)
					}
					if num, err := strconv.Atoi(parts[1]); err != nil {
						return fmt.Errorf("invalid number for ndots option: %v", parts[1])
					} else if num >= 0 {
						// if the user sets ndots, use the user setting
						sb.ndotsSet = true
						break dnsOpt
					} else {
						return fmt.Errorf("invalid number for ndots option: %v", num)
					}
				}
			}
		}
	}

	if !sb.ndotsSet {
		// if the user did not set the ndots, set it to 0 to prioritize the service name resolution
		// Ref: https://linux.die.net/man/5/resolv.conf
		dnsOptionsList = append(dnsOptionsList, resOptions...)
	}
	if len(sb.extDNS) == 0 {
		sb.setExternalResolvers(currRC, resolvconf.IPv4, false)
	}

	var (
		// external v6 DNS servers have to be listed in resolv.conf
		dnsList       = append([]string{sb.resolver.NameServer()}, resolvconf.GetNameservers(currRC, resolvconf.IPv6)...)
		dnsSearchList = resolvconf.GetSearchDomains(currRC)
	)

	_, err = resolvconf.Build(sb.config.resolvConfPath, dnsList, dnsSearchList, dnsOptionsList)
	return err
}

func createBasePath(dir string) error {
	return os.MkdirAll(dir, dirPerm)
}

func createFile(path string) error {
	var f *os.File

	dir, _ := filepath.Split(path)
	err := createBasePath(dir)
	if err != nil {
		return err
	}

	f, err = os.Create(path)
	if err == nil {
		f.Close()
	}

	return err
}

func copyFile(src, dst string) error {
	sBytes, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, sBytes, filePerm)
}
