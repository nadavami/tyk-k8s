package tyk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/TykTechnologies/tyk-git/clients/dashboard"
	"github.com/TykTechnologies/tyk-git/clients/objects"
	"github.com/TykTechnologies/tyk-k8s/logger"
	"github.com/TykTechnologies/tyk-k8s/processor"
	"github.com/spf13/viper"
	"path"
	"regexp"
	"strings"
	"text/template"
)

func cleanSlug(s string) string {
	r, _ := regexp.Compile("[^a-zA-Z0-9-_/.]")
	s = r.ReplaceAllString(s, "")
	r2, _ := regexp.Compile("(//+)")
	s = r2.ReplaceAllString(s, "")
	//trim ends:
	s = strings.Trim(s, "/")

	if len(s) == 0 {
		s = "0"
	}
	return s
}

type TykConf struct {
	URL       string `yaml:"url"`
	Secret    string `yaml:"secret"`
	Org       string `yaml:"org"`
	Templates string `yaml:"templates"`
}

type APIDefOptions struct {
	Name         string
	Target       string
	ListenPath   string
	TemplateName string
	Hostname     string
	Slug         string
	Tags         []string
	APIID        string
	ID           string
	LegacyAPIDef *dashboard.DBApiDefinition
	Annotations  map[string]string
}

var cfg *TykConf
var log = logger.GetLogger("tyk-api")
var templates *template.Template
var defaultTemplate *template.Template

const (
	DefaultTemplate = "default"
)

func Init(forceConf *TykConf) {
	defaultTemplate = template.Must(template.New("default").Parse(defaultAPITemplate))

	if forceConf != nil {
		cfg = forceConf
		return
	}

	if cfg == nil {
		cfg = &TykConf{}
		err := viper.UnmarshalKey("Tyk", cfg)
		if err != nil {
			log.Fatalf("failed to load config: %v", err)
		}
	}

	if cfg.Templates != "" {
		templates = template.Must(template.ParseGlob(path.Join(cfg.Templates, "*")))
	}

}

func newClient() *dashboard.Client {
	cl, err := dashboard.NewDashboardClient(cfg.URL, cfg.Secret)
	if err != nil {
		log.Fatalf("failed to create tyk dashboard client: %v", err)
	}

	return cl
}

func getTemplate(name string) (*template.Template, error) {
	if cfg.Templates == "" {
		log.Warning("using default template")
		return defaultTemplate, nil
	}

	if templates == nil {
		return defaultTemplate, errors.New("no templates loaded")
	}

	tpl := templates.Lookup(name)
	if tpl == nil {
		return defaultTemplate, errors.New("not found")
	}

	return tpl, nil

}

func TemplateService(opts *APIDefOptions) ([]byte, error) {
	if opts.TemplateName == "" {
		opts.TemplateName = DefaultTemplate
	}

	defTpl, err := getTemplate(opts.TemplateName)
	if err != nil {
		return nil, err
	}

	tplVars := map[string]interface{}{
		"Name":        opts.Name,
		"Slug":        cleanSlug(opts.Slug),
		"Org":         cfg.Org,
		"ListenPath":  opts.ListenPath,
		"Target":      opts.Target,
		"GatewayTags": opts.Tags,
		"HostName":    opts.Hostname,
	}

	var apiDefStr bytes.Buffer
	err = defTpl.Execute(&apiDefStr, tplVars)
	if err != nil {
		return nil, err
	}

	return apiDefStr.Bytes(), nil
}

func CreateService(opts *APIDefOptions) (string, error) {
	adBytes, err := TemplateService(opts)
	if err != nil {
		return "", err
	}

	postProcessedDef := string(adBytes)
	if opts.Annotations != nil {
		postProcessedDef, err = processor.Process(opts.Annotations, string(adBytes))
		if err != nil {
			return "", err
		}
	}

	apiDef := objects.NewDefinition()
	err = json.Unmarshal([]byte(postProcessedDef), apiDef)
	if err != nil {
		return "", err
	}

	cl := newClient()

	return cl.CreateAPI(apiDef)

}

func DeleteBySlug(slug string) error {
	cl := newClient()

	allServices, err := cl.FetchAPIs()
	if err != nil {
		return err
	}

	cSlug := cleanSlug(slug)
	for _, s := range allServices {
		if cSlug == s.Slug {
			log.Warning("found API entry, deleting: ", s.Id.Hex())
			return cl.DeleteAPI(s.Id.Hex())
		}
	}

	return fmt.Errorf("service with name %s not found for removal, remove manually", slug)
}

func UpdateAPIs(svcs map[string]*APIDefOptions) error {
	cl := newClient()

	allServices, err := cl.FetchAPIs()
	if err != nil {
		return err
	}

	errs := make([]error, 0)
	toUpdate := map[string]*APIDefOptions{}
	toCreate := map[string]*APIDefOptions{}

	// To update
	for ingressID, o := range svcs {
		cSlug := cleanSlug(ingressID)
		for _, s := range allServices {
			if cSlug == s.Slug {
				o.LegacyAPIDef = &s
				toUpdate[cSlug] = o
			}
		}
	}

	// To create
	for ingressID, o := range svcs {
		cSlug := cleanSlug(ingressID)
		_, updatingAlready := toUpdate[cSlug]
		if updatingAlready {
			// skip
			continue
		}

		toCreate[cSlug] = o
	}

	for _, opts := range toUpdate {
		adBytes, err := TemplateService(opts)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		apiDef := objects.NewDefinition()
		err = json.Unmarshal(adBytes, apiDef)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		// Retain identity
		apiDef.Id = opts.LegacyAPIDef.Id
		apiDef.APIID = opts.LegacyAPIDef.APIID
		apiDef.OrgID = opts.LegacyAPIDef.OrgID

		err = cl.UpdateAPI(apiDef)
		if err != nil {
			errs = append(errs, err)
			continue
		}

	}

	for _, opts := range toCreate {
		id, err := CreateService(opts)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		log.Info("created: ", id)
	}

	if len(errs) > 0 {
		msg := ""
		for i, e := range errs {
			if i != 0 {
				msg = e.Error()
			}
			msg += "; " + msg
		}

		return fmt.Errorf(msg)
	}

	return nil

}

func GetBySlug(slug string) (*dashboard.DBApiDefinition, error) {
	cl := newClient()

	allServices, err := cl.FetchAPIs()
	if err != nil {
		return nil, err
	}

	cSlug := cleanSlug(slug)
	for _, s := range allServices {
		if cSlug == s.Slug {
			return &s, nil
		}
	}

	return nil, fmt.Errorf("service with name %s not found", slug)
}

func DeleteByID(id string) error {
	cl := newClient()
	return cl.DeleteAPI(id)
}

var defaultAPITemplate = `
{
    "name": "{{.Name}}{{ range $i, $e := .GatewayTags }} #{{$e}}{{ end }}",
	"slug": "{{.Slug}}",
    "org_id": "{{.Org}}",
    "use_keyless": true,
    "definition": {
        "location": "header",
        "key": "x-api-version",
        "strip_path": true
    },
    "version_data": {
        "not_versioned": true,
        "versions": {
            "Default": {
                "name": "Default",
                "use_extended_paths": true,
				"global_headers": {
                    "X-Tyk-Request-ID": "$tyk_context.request_id"
                },
				"paths": {
                    "ignored": [],
                    "white_list": [],
                    "black_list": []
                }
            }
        }
    },
    "proxy": {
        "listen_path": "{{.ListenPath}}",
        "target_url": "{{.Target}}",
        "strip_listen_path": true
    },
	"domain": "{{.HostName}}",
	"response_processors": [],
	 "custom_middleware": {
        "pre": [],
        "post": [],
        "post_key_auth": [],
        "auth_check": {
            "name": "",
            "path": "",
            "require_session": false
        },
        "response": [],
        "driver": "",
        "id_extractor": {
            "extract_from": "",
            "extract_with": "",
            "extractor_config": {}
        }
    },
	"config_data": {},
	"allowed_ips": [],
    "disable_rate_limit": true,
    "disable_quota": true,
    "cache_options": {
        "cache_timeout": 60,
        "enable_cache": true
    },
    "active": true,
    "tags": [{{ range $i, $e := .GatewayTags }}{{ if $i }},{{ end }}"{{ $e }}"{{ end }}],
    "enable_context_vars": true
}
`
