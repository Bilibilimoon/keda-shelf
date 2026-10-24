package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".gif": true, ".bmp": true, ".avif": true, ".heic": true,
}

var archiveExts = map[string]string{
	".cbz": "cbz", ".zip": "zip", ".cbr": "cbr", ".rar": "rar",
	".cb7": "cb7", ".pdf": "pdf", ".epub": "epub",
}

func extOf(name string) string { return strings.ToLower(filepath.Ext(name)) }

func isImageName(name string) bool { return imageExts[extOf(name)] }

func archiveFormat(name string) string { return archiveExts[extOf(name)] }

func hashID(rel string) string {
	sum := sha1.Sum([]byte(rel))
	return hex.EncodeToString(sum[:])[:16]
}

func contentTypeFor(name string) string {
	switch extOf(name) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".avif":
		return "image/avif"
	case ".cbz", ".zip":
		return "application/vnd.comicbook+zip"
	case ".cbr", ".rar":
		return "application/vnd.comicbook-rar"
	case ".pdf":
		return "application/pdf"
	case ".epub":
		return "application/epub+zip"
	default:
		return "application/octet-stream"
	}
}

func opdsTypeFor(name string) string { return contentTypeFor(name) }

// ignoreEntry reports whether a file or directory should be skipped during scans.
func ignoreEntry(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	switch strings.ToLower(name) {
	case "@eadir", "#recycle", "#snapshot", "$recycle.bin",
		"system volume information", "lost+found", "thumbs.db",
		"desktop.ini", "@recycle", "@tmp":
		return true
	}
	return false
}

const logTailMaxBytes = 128 * 1024

// readLogTail returns at most maxLines trailing lines of file, reading no
// more than logTailMaxBytes from the end so huge logs stay cheap to serve.
func readLogTail(file string, maxLines int) []string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	off := int64(0)
	if st.Size() > logTailMaxBytes {
		off = st.Size() - logTailMaxBytes
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

func detectLANIP() string {
	if ip := defaultRouteIP(); ip != "" {
		return ip
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var candidates []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "virbr") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			candidates = append(candidates, ip.String())
		}
	}
	sort.Strings(candidates)
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// defaultRouteIP prefers the address of the interface carrying the default
// route, so container bridges do not win over the real LAN address.
func defaultRouteIP() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if first {
			first = false
			continue
		}
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		iface, err := net.InterfaceByName(fields[0])
		if err != nil {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
					return ip4.String()
				}
			}
		}
	}
	return ""
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n >= div*unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
