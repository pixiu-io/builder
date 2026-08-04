package serve

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sassoftware/go-rpmutils"
)

// prepareRepos 将 rpm/deb 放入 repos 并生成索引。
func prepareRepos(roots []string, pkgRoot string) (rpmN, debN int, err error) {
	rpms, debs, err := collectPackages(roots)
	if err != nil {
		return 0, 0, err
	}
	rpmDir := filepath.Join(pkgRoot, "rpm")
	debDir := filepath.Join(pkgRoot, "deb")

	if len(rpms) > 0 {
		if err := os.MkdirAll(rpmDir, 0o755); err != nil {
			return 0, 0, err
		}
		var linked []string
		for _, src := range rpms {
			dst := filepath.Join(rpmDir, filepath.Base(src))
			if err := linkOrCopy(src, dst); err != nil {
				return 0, 0, fmt.Errorf("放置 rpm %s 失败: %w", src, err)
			}
			linked = append(linked, dst)
		}
		if err := writeRPMRepo(rpmDir, linked); err != nil {
			return 0, 0, err
		}
		rpmN = len(linked)
		fmt.Printf("  RPM 源: %d 个包 → %s\n", rpmN, rpmDir)
	}

	if len(debs) > 0 {
		if err := os.MkdirAll(debDir, 0o755); err != nil {
			return 0, 0, err
		}
		var linked []string
		for _, src := range debs {
			dst := filepath.Join(debDir, filepath.Base(src))
			if err := linkOrCopy(src, dst); err != nil {
				return 0, 0, fmt.Errorf("放置 deb %s 失败: %w", src, err)
			}
			linked = append(linked, dst)
		}
		if err := writeDebRepo(debDir, linked); err != nil {
			return 0, 0, err
		}
		debN = len(linked)
		fmt.Printf("  Deb 源: %d 个包 → %s\n", debN, debDir)
	}

	if rpmN == 0 && debN == 0 {
		fmt.Println("  未发现 .rpm/.deb，跳过软件源")
	}
	return rpmN, debN, nil
}

type rpmPkg struct {
	Name        string
	Arch        string
	Epoch       string
	Version     string
	Release     string
	Summary     string
	Description string
	URL         string
	License     string
	Vendor      string
	Group       string
	BuildHost   string
	SourceRPM   string
	PkgID       string
	Href        string
	TimeFile    int64
	TimeBuild   int64
	SizePkg     int64
	SizeInst    int64
	SizeArchive int64
	HdrStart    int
	HdrEnd      int
	Provides    []rpmEntry
	Requires    []rpmEntry
	Conflicts   []rpmEntry
	Obsoletes   []rpmEntry
	Files       []rpmFile
}

type rpmEntry struct {
	Name  string
	Flags string
	Epoch string
	Ver   string
	Rel   string
}

type rpmFile struct {
	Path string
	Type string
}

type repoMetaBlob struct {
	Type         string
	Checksum     string
	OpenChecksum string
	Location     string
	Timestamp    int64
	Size         int64
	OpenSize     int64
}

func writeRPMRepo(rpmDir string, rpmFiles []string) error {
	var pkgs []rpmPkg
	for _, path := range rpmFiles {
		p, err := parseRPM(path, filepath.Base(path))
		if err != nil {
			return fmt.Errorf("解析 %s: %w", path, err)
		}
		pkgs = append(pkgs, p)
	}

	repoData := filepath.Join(rpmDir, "repodata")
	if err := os.MkdirAll(repoData, 0o755); err != nil {
		return err
	}
	entries, _ := os.ReadDir(repoData)
	for _, e := range entries {
		_ = os.Remove(filepath.Join(repoData, e.Name()))
	}

	blobs := []struct {
		Type string
		XML  []byte
	}{
		{Type: "primary", XML: buildPrimaryXML(pkgs)},
		{Type: "filelists", XML: buildFilelistsXML(pkgs)},
		{Type: "other", XML: buildOtherXML(pkgs)},
	}

	ts := time.Now().Unix()
	var metas []repoMetaBlob
	for _, f := range blobs {
		gzName := f.Type + ".xml.gz"
		gzPath := filepath.Join(repoData, gzName)
		gzData, err := gzipBytes(f.XML)
		if err != nil {
			return err
		}
		if err := os.WriteFile(gzPath, gzData, 0o644); err != nil {
			return err
		}
		metas = append(metas, repoMetaBlob{
			Type:         f.Type,
			Checksum:     sha256Hex(gzData),
			OpenChecksum: sha256Hex(f.XML),
			Location:     "repodata/" + gzName,
			Timestamp:    ts,
			Size:         int64(len(gzData)),
			OpenSize:     int64(len(f.XML)),
		})
	}
	return os.WriteFile(filepath.Join(repoData, "repomd.xml"), buildRepomdXML(metas), 0o644)
}

func parseRPM(path, href string) (rpmPkg, error) {
	f, err := os.Open(path)
	if err != nil {
		return rpmPkg{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return rpmPkg{}, err
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return rpmPkg{}, err
	}

	rpm, err := rpmutils.ReadRpm(f)
	if err != nil {
		return rpmPkg{}, err
	}
	h := rpm.Header
	hr := h.GetRange()

	sizeInst, _ := h.InstalledSize()
	sizeArchive, _ := h.PayloadSize()

	pkg := rpmPkg{
		Name:        strTag(h, rpmutils.NAME),
		Arch:        strTag(h, rpmutils.ARCH),
		Epoch:       epochTag(h),
		Version:     strTag(h, rpmutils.VERSION),
		Release:     strTag(h, rpmutils.RELEASE),
		Summary:     strTag(h, rpmutils.SUMMARY),
		Description: strTag(h, rpmutils.DESCRIPTION),
		URL:         strTag(h, rpmutils.URL),
		License:     strTag(h, rpmutils.LICENSE),
		Vendor:      strTag(h, rpmutils.VENDOR),
		Group:       strTag(h, rpmutils.GROUP),
		BuildHost:   strTag(h, rpmutils.BUILDHOST),
		SourceRPM:   strTag(h, rpmutils.SOURCERPM),
		PkgID:       sum,
		Href:        href,
		TimeFile:    st.ModTime().Unix(),
		TimeBuild:   int64(intTag(h, rpmutils.BUILDTIME)),
		SizePkg:     st.Size(),
		SizeInst:    sizeInst,
		SizeArchive: sizeArchive,
		HdrStart:    hr.Start,
		HdrEnd:      hr.End,
		Provides:    entriesFrom(h, rpmutils.PROVIDENAME, rpmutils.PROVIDEFLAGS, rpmutils.PROVIDEVERSION),
		Requires:    entriesFrom(h, rpmutils.REQUIRENAME, rpmutils.REQUIREFLAGS, rpmutils.REQUIREVERSION),
		Conflicts:   entriesFrom(h, rpmutils.CONFLICTNAME, 0, rpmutils.CONFLICTVERSION),
		Obsoletes:   entriesFrom(h, rpmutils.OBSOLETENAME, 0, rpmutils.OBSOLETEVERSION),
	}

	if files, err := h.GetFiles(); err == nil {
		for _, fi := range files {
			p := fi.Name()
			if p == "" {
				continue
			}
			t := ""
			if fi.Mode()&0o170000 == 0o040000 {
				t = "dir"
			}
			pkg.Files = append(pkg.Files, rpmFile{Path: p, Type: t})
		}
	}
	return pkg, nil
}

func strTag(h *rpmutils.RpmHeader, tag int) string {
	s, err := h.GetString(tag)
	if err != nil {
		return ""
	}
	return s
}

func intTag(h *rpmutils.RpmHeader, tag int) int {
	v, err := h.GetInt(tag)
	if err != nil {
		return 0
	}
	return v
}

func epochTag(h *rpmutils.RpmHeader) string {
	if !h.HasTag(rpmutils.EPOCH) {
		return "0"
	}
	return strconv.Itoa(intTag(h, rpmutils.EPOCH))
}

func entriesFrom(h *rpmutils.RpmHeader, nameTag, flagsTag, versionTag int) []rpmEntry {
	names, err := h.GetStrings(nameTag)
	if err != nil || len(names) == 0 {
		return nil
	}
	var flags []uint32
	if flagsTag != 0 {
		flags, _ = h.GetUint32s(flagsTag)
	}
	versions, _ := h.GetStrings(versionTag)
	out := make([]rpmEntry, 0, len(names))
	for i, n := range names {
		e := rpmEntry{Name: n, Epoch: "0"}
		if i < len(flags) {
			e.Flags = senseFlag(int(flags[i]))
		}
		if i < len(versions) {
			e.Epoch, e.Ver, e.Rel = splitEVR(versions[i])
		}
		out = append(out, e)
	}
	return out
}

func senseFlag(f int) string {
	const (
		less    = 0x02
		greater = 0x04
		equal   = 0x08
	)
	switch {
	case f&less != 0 && f&equal != 0:
		return "LE"
	case f&greater != 0 && f&equal != 0:
		return "GE"
	case f&less != 0:
		return "LT"
	case f&greater != 0:
		return "GT"
	case f&equal != 0:
		return "EQ"
	default:
		return ""
	}
}

func splitEVR(evr string) (epoch, ver, rel string) {
	epoch = "0"
	if i := strings.Index(evr, ":"); i >= 0 {
		epoch = evr[:i]
		evr = evr[i+1:]
	}
	if i := strings.Index(evr, "-"); i >= 0 {
		ver = evr[:i]
		rel = evr[i+1:]
	} else {
		ver = evr
	}
	return
}

func buildPrimaryXML(pkgs []rpmPkg) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(fmt.Sprintf(`<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="%d">`, len(pkgs)) + "\n")
	for _, p := range pkgs {
		b.WriteString(`  <package type="rpm">` + "\n")
		b.WriteString("    <name>" + xmlEscape(p.Name) + "</name>\n")
		b.WriteString("    <arch>" + xmlEscape(p.Arch) + "</arch>\n")
		b.WriteString(fmt.Sprintf(`    <version epoch="%s" ver="%s" rel="%s"/>`, xmlEscape(p.Epoch), xmlEscape(p.Version), xmlEscape(p.Release)) + "\n")
		b.WriteString(fmt.Sprintf(`    <checksum type="sha256" pkgid="YES">%s</checksum>`, p.PkgID) + "\n")
		b.WriteString("    <summary>" + xmlEscape(p.Summary) + "</summary>\n")
		b.WriteString("    <description>" + xmlEscape(p.Description) + "</description>\n")
		b.WriteString("    <packager>" + xmlEscape(p.Vendor) + "</packager>\n")
		b.WriteString("    <url>" + xmlEscape(p.URL) + "</url>\n")
		b.WriteString(fmt.Sprintf(`    <time file="%d" build="%d"/>`, p.TimeFile, p.TimeBuild) + "\n")
		b.WriteString(fmt.Sprintf(`    <size package="%d" installed="%d" archive="%d"/>`, p.SizePkg, p.SizeInst, p.SizeArchive) + "\n")
		b.WriteString(fmt.Sprintf(`    <location href="%s"/>`, xmlEscape(p.Href)) + "\n")
		b.WriteString("    <format>\n")
		b.WriteString("      <rpm:license>" + xmlEscape(p.License) + "</rpm:license>\n")
		b.WriteString("      <rpm:vendor>" + xmlEscape(p.Vendor) + "</rpm:vendor>\n")
		b.WriteString("      <rpm:group>" + xmlEscape(p.Group) + "</rpm:group>\n")
		b.WriteString("      <rpm:buildhost>" + xmlEscape(p.BuildHost) + "</rpm:buildhost>\n")
		b.WriteString("      <rpm:sourcerpm>" + xmlEscape(p.SourceRPM) + "</rpm:sourcerpm>\n")
		b.WriteString(fmt.Sprintf(`      <rpm:header-range start="%d" end="%d"/>`, p.HdrStart, p.HdrEnd) + "\n")
		writeEntryList(&b, "provides", p.Provides)
		writeEntryList(&b, "requires", p.Requires)
		writeEntryList(&b, "conflicts", p.Conflicts)
		writeEntryList(&b, "obsoletes", p.Obsoletes)
		b.WriteString("    </format>\n")
		b.WriteString("  </package>\n")
	}
	b.WriteString("</metadata>\n")
	return []byte(b.String())
}

func writeEntryList(b *strings.Builder, kind string, entries []rpmEntry) {
	if len(entries) == 0 {
		return
	}
	b.WriteString("      <rpm:" + kind + ">\n")
	for _, e := range entries {
		b.WriteString(`        <rpm:entry name="` + xmlEscape(e.Name) + `"`)
		if e.Flags != "" {
			b.WriteString(` flags="` + e.Flags + `"`)
		}
		if e.Ver != "" {
			b.WriteString(` epoch="` + xmlEscape(e.Epoch) + `" ver="` + xmlEscape(e.Ver) + `"`)
			if e.Rel != "" {
				b.WriteString(` rel="` + xmlEscape(e.Rel) + `"`)
			}
		}
		b.WriteString("/>\n")
	}
	b.WriteString("      </rpm:" + kind + ">\n")
}

func buildFilelistsXML(pkgs []rpmPkg) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(fmt.Sprintf(`<filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="%d">`, len(pkgs)) + "\n")
	for _, p := range pkgs {
		b.WriteString(fmt.Sprintf(`  <package pkgid="%s" name="%s" arch="%s">`, p.PkgID, xmlEscape(p.Name), xmlEscape(p.Arch)) + "\n")
		b.WriteString(fmt.Sprintf(`    <version epoch="%s" ver="%s" rel="%s"/>`, xmlEscape(p.Epoch), xmlEscape(p.Version), xmlEscape(p.Release)) + "\n")
		for _, f := range p.Files {
			if f.Type != "" {
				b.WriteString(fmt.Sprintf(`    <file type="%s">%s</file>`, f.Type, xmlEscape(f.Path)) + "\n")
			} else {
				b.WriteString("    <file>" + xmlEscape(f.Path) + "</file>\n")
			}
		}
		b.WriteString("  </package>\n")
	}
	b.WriteString("</filelists>\n")
	return []byte(b.String())
}

func buildOtherXML(pkgs []rpmPkg) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(fmt.Sprintf(`<otherdata xmlns="http://linux.duke.edu/metadata/other" packages="%d">`, len(pkgs)) + "\n")
	for _, p := range pkgs {
		b.WriteString(fmt.Sprintf(`  <package pkgid="%s" name="%s" arch="%s">`, p.PkgID, xmlEscape(p.Name), xmlEscape(p.Arch)) + "\n")
		b.WriteString(fmt.Sprintf(`    <version epoch="%s" ver="%s" rel="%s"/>`, xmlEscape(p.Epoch), xmlEscape(p.Version), xmlEscape(p.Release)) + "\n")
		b.WriteString("  </package>\n")
	}
	b.WriteString("</otherdata>\n")
	return []byte(b.String())
}

func buildRepomdXML(metas []repoMetaBlob) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n")
	for _, m := range metas {
		b.WriteString(fmt.Sprintf(`  <data type="%s">`, m.Type) + "\n")
		b.WriteString(fmt.Sprintf(`    <checksum type="sha256">%s</checksum>`, m.Checksum) + "\n")
		b.WriteString(fmt.Sprintf(`    <open-checksum type="sha256">%s</open-checksum>`, m.OpenChecksum) + "\n")
		b.WriteString(fmt.Sprintf(`    <location href="%s"/>`, xmlEscape(m.Location)) + "\n")
		b.WriteString(fmt.Sprintf(`    <timestamp>%d</timestamp>`, m.Timestamp) + "\n")
		b.WriteString(fmt.Sprintf(`    <size>%d</size>`, m.Size) + "\n")
		b.WriteString(fmt.Sprintf(`    <open-size>%d</open-size>`, m.OpenSize) + "\n")
		b.WriteString("  </data>\n")
	}
	b.WriteString("</repomd>\n")
	return []byte(b.String())
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
