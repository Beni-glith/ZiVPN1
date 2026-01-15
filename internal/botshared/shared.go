package botshared

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type IpInfo struct {
	City  string `json:"city"`
	Isp   string `json:"isp"`
	Query string `json:"query"`
}

const (
	BotConfigFile = "/etc/zivpn/bot-config.json"
	ApiPortFile   = "/etc/zivpn/api_port"
	ApiKeyFile    = "/etc/zivpn/apikey"
	DomainFile    = "/etc/zivpn/domain"
	PortFile      = "/etc/zivpn/port"
)

var ApiUrl = "http://127.0.0.1:" + PortFile + "/api"

var ApiKey = "AutoFtBot-agskjgdvsbdreiWG1234512SDKrqw"

type UserData struct {
	Password     string `json:"password"`
	Expired      string `json:"expired"`
	Status       string `json:"status"`
	IpLimit      int    `json:"ip_limit"`
	ManualLocked bool   `json:"manual_locked"`
}

func ApiCall(method, endpoint string, payload interface{}) (map[string]interface{}, error) {
	var reqBody []byte
	var err error

	if payload != nil {
		reqBody, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}

	client := &http.Client{}
	req, err := http.NewRequest(method, ApiUrl+endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", ApiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	return result, nil
}

func GetIpInfo() (IpInfo, error) {
	resp, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return IpInfo{}, err
	}
	defer resp.Body.Close()

	var info IpInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return IpInfo{}, err
	}
	return info, nil
}

func GetUsers() ([]UserData, error) {
	res, err := ApiCall("GET", "/users", nil)
	if err != nil {
		return nil, err
	}

	if res["success"] != true {
		return nil, fmt.Errorf("failed to get users")
	}

	var users []UserData
	dataBytes, _ := json.Marshal(res["data"])
	json.Unmarshal(dataBytes, &users)
	return users, nil
}
