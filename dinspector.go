package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	kivik "github.com/go-kivik/kivik/v3"
	_ "github.com/go-kivik/couchdb/v3" // The CouchDB driver
	"gopkg.in/ini.v1"
)

func IsEmptyDir(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err // not empty or other error
}

type ItemWithSubKind struct {
	id      string
	kind    string
	subKind string
	subLoc  string
}

type ItemVersion struct {
	Domain           string `json:"domain"`
	Kind             string `json:"type"`
	KindTitle        string `json:"title"`
	Path             string `json:"path"`
	Version          string `json:"version"`
	PackagesVersions string `json:"packages_versions,omitempty"`
}

type ItemNotFound struct {
	Domain string `json:"domain"`
	Kind   string `json:"type"`
	Path   string `json:"path"`
}

type ItemVersionDoc struct {
	ID            string         `json:"_id"`
	Rev           string         `json:"_rev,omitempty"`
	DocType       string         `json:"type"`
	DocSubType    string         `json:"sub_type"`
	Hostname      string         `json:"hostname"`
	Items         []ItemVersion  `json:"items"`
	ItemsNotFound []ItemNotFound `json:"items_not_found"`
}

type Inspector struct {
	config          ini.File
	db              kivik.DB
	scriptsPath     string
	isDryRunVerbose bool
	itemsVersion    []ItemVersion
	itemsNotFound   []ItemNotFound
}

func (inspector *Inspector) Init() {
	// config
	configPath := flag.String(
		"config",
		"/etc/desk/inspector.conf",
		"path of the inspector.conf config file (default: /etc/desk/inspector.conf)",
	)
	isDryRunVerbose := flag.Bool(
		"n",
		false,
		"only output, no save",
	)
	flag.Parse()
	cfg, err := ini.Load(*configPath)
	if err != nil {
		panic(err)
	}
	inspector.config = *cfg
	inspector.scriptsPath = cfg.Section("inspector").Key("scripts").String()
	inspector.isDryRunVerbose = *isDryRunVerbose

	// db
	client, err := kivik.New("couch", cfg.Section("couchdb").Key("uri").String())
	if err != nil {
		panic(err)
	}
	db := client.DB(context.TODO(), cfg.Section("couchdb").Key("db").String())
	inspector.db = *db
}

func (inspector *Inspector) getDoc(docContainer interface{}, ID string) error {
	row := inspector.db.Get(context.TODO(), ID)
	return nil
}

func (inspector *Inspector) processWebItems() {
	rows, err := inspector.db.Query(
		context.TODO(), "_design/desk_drawer", "_view/service_type",
		map[string]interface{}{"startkey": `["web"]`, "endkey": `["web"]`},
	)
	if err != nil {
		panic(err)
	}
	for rows.Next() {
		var doc interface{}
		if err := rows.ScanValue(&doc); err != nil {
			panic(err)
		}
		docMap := doc.(map[string]interface{})
		if included_service_items, ok := docMap["included_service_items"]; ok {
			for _, itemFromJson := range included_service_items.([]interface{}) {
				itemMap := itemFromJson.(map[string]interface{})
				if itemMap["itemSubType"] != nil && itemMap["itemSubLoc"] != nil {
					item := ItemWithSubKind{
						id:      itemMap["itemid"].(string),
						kind:    itemMap["itemType"].(string),
						subKind: itemMap["itemSubType"].(string),
						subLoc:  strings.TrimSpace(itemMap["itemSubLoc"].(string)),
					}
					inspector.checkWebVersion(item)
				}
			}
		}
	}
	if rows.Err() != nil {
		panic(rows.Err())
	}
}

func (inspector *Inspector) checkWebVersion(item ItemWithSubKind) {
	scriptPath := fmt.Sprint(inspector.scriptsPath, "/", item.subKind, ".sh")
	isEmptySubLocDir, _ := IsEmptyDir(strings.TrimSpace(item.subLoc))
	if _, err := os.Stat(scriptPath); !os.IsNotExist(err) && !isEmptySubLocDir {
		cmd := exec.Command(scriptPath)
		cmd.Dir = strings.TrimSpace(item.subLoc)
		versionOutput, err := cmd.Output()
		pass := true
		if err != nil {
			if strings.Index(fmt.Sprint(err), "chdir") >= 0 {
				pass = false
				if inspector.isDryRunVerbose {
					fmt.Printf("!chdir not found:%s\n", item.subLoc)
				}
				newItemNotFound := ItemNotFound{
					Domain: item.id,
					Kind:   item.subKind,
					Path:   item.subLoc,
				}
				inspector.itemsNotFound = append(inspector.itemsNotFound, newItemNotFound)
			} else {
				panic(err)
			}
		}
		if pass {
			versionString := strings.TrimSpace(string(versionOutput[:]))
			versionParts := strings.Split(versionString, "|")
			KindTitle := strings.TrimSpace(
				inspector.config.Section("inspector_scripts").Key(item.subKind).String(),
			)
			newItemVersion := ItemVersion{
				Domain:    item.id,
				Kind:      item.subKind,
				KindTitle: KindTitle,
				Path:      item.subLoc,
				Version:   versionParts[0],
			}
			if len(versionParts) == 2 {
				newItemVersion.PackagesVersions = versionParts[1]
			}
			inspector.itemsVersion = append(inspector.itemsVersion, newItemVersion)
		}
	}
}

func (inspector *Inspector) putItemVersionDoc(id string, rev string, hostname string) {
	itemVersionDoc := ItemVersionDoc{
		ID:            id,
		Rev:           rev,
		Hostname:      hostname,
		DocType:       "inspector",
		DocSubType:    "web",
		Items:         inspector.itemsVersion,
		ItemsNotFound: inspector.itemsNotFound,
	}
	_, err := inspector.db.Put(context.TODO(), id, itemVersionDoc)
	if err != nil {
		panic(err)
	}
	// return itemVersionDoc
}

func (inspector *Inspector) printWebVersions() {
	for _, item := range inspector.itemsVersion {
		versions := item.Version
		if item.PackagesVersions != "" {
			versions = versions + "; " + item.PackagesVersions
		}
		fmt.Printf("- %s:%s - %s\n  %s\n", item.Domain, item.Kind, item.KindTitle, versions)
	}
}

func (inspector *Inspector) saveWebVersions() {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	id := fmt.Sprintf("%s-%s", "inspector", hostname)
	_, docRev, err := inspector.db.GetMeta(context.TODO(), id)
	if err != nil {
		if kivik.StatusCode(err) == kivik.StatusNotFound {
			inspector.putItemVersionDoc(id, "", hostname)
		} else {
			panic(err)
		}
	} else {
		inspector.putItemVersionDoc(id, docRev, hostname)
	}
}

func main() {
	inspector := Inspector{}
	inspector.Init()
	inspector.processWebItems()
	if inspector.isDryRunVerbose {
		inspector.printWebVersions()
	} else {
		inspector.saveWebVersions()
	}
}
