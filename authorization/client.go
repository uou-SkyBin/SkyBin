package authorization

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
)

func NewClient(addr string, client *http.Client) *Client {
	return &Client{
		addr:   addr,
		client: client,
	}
}

type Client struct {
	addr   string
	client *http.Client
}

func (client *Client) GetAuthToken(privateKey *rsa.PrivateKey, authType string, userID string) (string, error) {
	challengeURL := fmt.Sprintf("http://%[1]s/auth/%[2]s?%[2]sID=%[3]s", client.addr, authType, userID)

	// Get a challenge token
	resp, err := client.client.Get(challengeURL)
	if err != nil {
		return "", err
	}
	var respMsg GetAuthChallengeResp
	_ = json.NewDecoder(resp.Body).Decode(&respMsg)
	token := respMsg.Nonce

	// Sign the token
	hashed := sha256.Sum256([]byte(token))

	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", err
	}

	// Encode the token and send it back to the server.
	encoded := base64.URLEncoding.EncodeToString(signature)

	respondURL := fmt.Sprintf("http://%[1]s/auth/%[2]s", client.addr, authType)
	responseField := fmt.Sprintf("%sID", authType)
	resp, err = client.client.PostForm(respondURL, url.Values{responseField: {userID}, "signedNonce": {encoded}})
	if err != nil {
		return "", err
	} else {
		println(resp.StatusCode)
		var b []byte
		defer resp.Body.Close()
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != 200 {
			println(string(b))
			panic("Bad status: " + resp.Status)
		}
		return string(b), nil
	}
}
