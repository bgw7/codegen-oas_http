package apiClient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pb33f/libopenapi/datamodel"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	"github.com/pb33f/libopenapi/datamodel/high/v3"
)

type Client struct {
	httpClient *http.Client
	authToken  string
}

func (c *Client) SetAuthToken(token string) {
	c.authToken = token
}

func (c *Client) addAuthHeader(req *http.Request) {
	if c.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+c.authToken)
	}
}

func loadOpenAPISpec(filePath string) (*v3.Document, error) {
	spec, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read OpenAPI spec: %w", err)
	}

	doc, diag, err := datamodel.NewOpenAPIDocument(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OpenAPI spec: %w", err)
	}

	if diag.HasErrors() {
		return nil, fmt.Errorf("OpenAPI spec contains errors: %v", diag)
	}

	return doc.(*v3.Document), nil
}

func generateStructsFromSchemas(doc *v3.Document) string {
	code := ""

	for schemaName, schema := range doc.Components.Schemas {
		code += fmt.Sprintf("type %s struct {\n", schemaName)

		for propName, propSchema := range schema.Schema().Properties {
			goType := mapComplexSchemaToGo(propSchema)
			code += fmt.Sprintf("\t%s %s `json:\"%s\"`\n", propName, goType, propName)
		}

		code += "}\n\n"
	}
	return code
}

func mapComplexSchemaToGo(schema *base.Schema) string {
	if schema == nil {
		return "interface{}"
	}
	switch schema.Type {
	case "object":
		return "map[string]interface{}"
	case "array":
		return "[]" + mapJSONSchemaTypeToGo(schema.Items)
	default:
		return mapJSONSchemaTypeToGo(schema)
	}
}

func mapJSONSchemaTypeToGo(schema *base.Schema) string {
	switch schema.Type {
	case "string":
		return "string"
	case "integer":
		return "int"
	case "boolean":
		return "bool"
	case "array":
		return "[]interface{}"
	default:
		return "interface{}"
	}
}

func generateEndpointFunction(funcName, path, method string, op *v3.Operation) string {
	code := fmt.Sprintf(`
// %s executes a %s request to %s
func (c *Client) %s(ctx context.Context, reqBody *RequestBodyStruct) (*ResponseStruct, error) {
	url := "%s"
	var req *http.Request
	var err error

	switch "%s" {
	case "GET":
		req, err = http.NewRequest("GET", url, nil)
	case "POST":
		reqBodyData, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		req, err = http.NewRequest("POST", url, bytes.NewBuffer(reqBodyData))
	case "PUT":
		reqBodyData, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		req, err = http.NewRequest("PUT", url, bytes.NewBuffer(reqBodyData))
	case "DELETE":
		req, err = http.NewRequest("DELETE", url, nil)
	}

	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
`, funcName, method, path, funcName, path, method)

	if op.Parameters != nil {
		for _, param := range op.Parameters {
			if param.In == "query" {
				code += fmt.Sprintf(`	req.URL.Query().Add("%s", reqBody.%s)
`, param.Name, param.Name)
			}
		}
	}

	code += `
	resp, err := c.doWithRetries(req, 3)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var response ResponseStruct
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return nil, err
	}

	return &response, nil
}
`
	return code
}

func generatePaginatedEndpointFunction(funcName, path, method string, op *v3.Operation) string {
	code := fmt.Sprintf(`
func (c *Client) %s(ctx context.Context, reqBody *RequestBodyStruct, limit, offset int) (*PaginatedResponse, error) {
	url := "%s"
	req, err := http.NewRequest("%s", url, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("limit", strconv.Itoa(limit))
	q.Add("offset", strconv.Itoa(offset))
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Content-Type", "application/json")
`, funcName, path, method)

	code += `
	resp, err := c.doWithRetries(req, 3)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var paginatedResponse PaginatedResponse
	err = json.NewDecoder(resp.Body).Decode(&paginatedResponse)
	if err != nil {
		return nil, err
	}

	return &paginatedResponse, nil
}
`
	return code
}

func (c *Client) doWithRetries(req *http.Request, maxRetries int) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempts := 0; attempts < maxRetries; attempts++ {
		resp, err = c.httpClient.Do(req)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		time.Sleep(time.Duration(attempts+1) * 100 * time.Millisecond)
	}
	return resp, err
}

func generateClientCode(doc *v3.Document) string {
	clientCode := "package apiClient\n\nimport (\n\t\"net/http\"\n\t\"context\"\n\t\"encoding/json\"\n\t\"strconv\"\n\t\"time\"\n\t\"bytes\"\n)\n\n"

	clientCode += `
type Client struct {
	httpClient *http.Client
	authToken  string
}

func (c *Client) SetAuthToken(token string) {
	c.authToken = token
}

func (c *Client) addAuthHeader(req *http.Request) {
	if c.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+c.authToken)
	}
}
`

	clientCode += generateStructsFromSchemas(doc)

	for path, item := range doc.Paths.PathItems {
		for method, op := range item.Operations() {
			funcName := fmt.Sprintf("%s%s", methodToFuncName(method), pathToFuncName(path))
			if supportsPagination(op) {
				clientCode += generatePaginatedEndpointFunction(funcName, path, method, op)
			} else {
				clientCode += generateEndpointFunction(funcName, path, method, op)
			}
		}
	}

	clientCode += `
func (c *Client) doWithRetries(req *http.Request, maxRetries int) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempts := 0; attempts < maxRetries; attempts++ {
		resp, err = c.httpClient.Do(req)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		time.Sleep(time.Duration(attempts+1) * 100 * time.Millisecond)
	}
	return resp, err
}
`
	return clientCode
}

func saveGeneratedCode(filename, code string) error {
	return os.WriteFile(filename, []byte(code), 0644)
}

func GenerateClientCode() {
	doc, err := loadOpenAPISpec("path/to/openapi.yaml")
	if err != nil {
		fmt.Println("Error loading spec:", err)
		return
	}

	clientCode := generateClientCode(doc)
	if err := saveGeneratedCode("client_gen.go", clientCode); err != nil {
		fmt.Println("Error saving generated code:", err)
		return
	}

	fmt.Println("Client code generated successfully in client_gen.go")
}

func methodToFuncName(method string) string {
	return strings.Title(strings.ToLower(method))
}

func pathToFuncName(path string) string {
	path = strings.ReplaceAll(path, "/", "")
	path = strings.ReplaceAll(path, "{", "By")
	path = strings.ReplaceAll(path, "}", "")
	return strings.Title(path)
}

func supportsPagination(op *v3.Operation) bool {
	for _, param := range op.Parameters {
		if param.Name == "limit" || param.Name == "offset" {
			return true
		}
	}
	return false
}
