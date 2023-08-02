/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apikeyhelper

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"github.com/apache/incubator-devlake/core/config"
	"github.com/apache/incubator-devlake/core/context"
	"github.com/apache/incubator-devlake/core/dal"
	"github.com/apache/incubator-devlake/core/errors"
	"github.com/apache/incubator-devlake/core/log"
	"github.com/apache/incubator-devlake/core/models"
	common "github.com/apache/incubator-devlake/core/models/common"
	"github.com/apache/incubator-devlake/core/utils"
	"github.com/spf13/viper"
	"regexp"
	"strings"
	"time"
)

const (
	EncodeKeyEnvStr = "ENCRYPTION_SECRET"
	apiKeyLen       = 128
)

type ApiKeyHelper struct {
	basicRes         context.BasicRes
	cfg              *viper.Viper
	logger           log.Logger
	encryptionSecret string
}

func NewApiKeyHelper(basicRes context.BasicRes, logger log.Logger) *ApiKeyHelper {
	cfg := config.GetConfig()
	encryptionSecret := strings.TrimSpace(cfg.GetString(EncodeKeyEnvStr))
	if encryptionSecret == "" {
		panic("ENCRYPTION_SECRET must be set in environment variable or .env file")
	}
	return &ApiKeyHelper{
		basicRes:         basicRes,
		cfg:              cfg,
		logger:           logger,
		encryptionSecret: encryptionSecret,
	}
}

func (c *ApiKeyHelper) Create(tx dal.Transaction, user *common.User, name string, expiredAt *time.Time, allowedPath string, apiKeyType string, extra string) (*models.ApiKey, errors.Error) {
	if _, err := regexp.Compile(allowedPath); err != nil {
		c.logger.Error(err, "Compile allowed path")
		return nil, errors.Default.Wrap(err, fmt.Sprintf("compile allowed path: %s", allowedPath))
	}
	apiKey, hashedApiKey, err := c.generateApiKey()
	if err != nil {
		c.logger.Error(err, "generateApiKey")
		return nil, err
	}
	now := time.Now()
	apiKeyRecord := &models.ApiKey{
		Model: common.Model{
			CreatedAt: now,
			UpdatedAt: now,
		},
		Name:        name,
		ApiKey:      hashedApiKey,
		ExpiredAt:   expiredAt,
		AllowedPath: allowedPath,
		Type:        apiKeyType,
		Extra:       extra,
	}
	if user != nil {
		apiKeyRecord.Creator = common.Creator{
			Creator:      user.Name,
			CreatorEmail: user.Email,
		}
		apiKeyRecord.Updater = common.Updater{
			Updater:      user.Name,
			UpdaterEmail: user.Email,
		}
	}
	if err := tx.Create(apiKeyRecord); err != nil {
		c.logger.Error(err, "create api key record")
		if tx.IsDuplicationError(err) {
			return nil, errors.BadInput.New(fmt.Sprintf("An api key with name [%s] has already exists", name))
		}
		return nil, errors.Default.Wrap(err, "error creating DB api key")
	}
	apiKeyRecord.ApiKey = apiKey
	return apiKeyRecord, nil
}

func (c *ApiKeyHelper) CreateForPlugin(tx dal.Transaction, user *common.User, name string, pluginName string, allowedPath string, extra string) (*models.ApiKey, errors.Error) {
	return c.Create(tx, user, name, nil, fmt.Sprintf("plugin:%s", pluginName), allowedPath, extra)
}

func (c *ApiKeyHelper) Put(user *common.User, id uint64) (*models.ApiKey, errors.Error) {
	db := c.basicRes.GetDal()
	// verify exists
	apiKey, err := c.getApiKeyById(db, id)
	if err != nil {
		c.logger.Error(err, "get api key by id: %d", id)
		return nil, err
	}

	apiKeyStr, hashApiKey, err := c.generateApiKey()
	if err != nil {
		c.logger.Error(err, "generateApiKey")
		return nil, err
	}
	apiKey.ApiKey = hashApiKey
	apiKey.UpdatedAt = time.Now()
	if user != nil {
		apiKey.Updater = common.Updater{
			Updater:      user.Name,
			UpdaterEmail: user.Email,
		}
	}
	if err = db.Update(apiKey); err != nil {
		c.logger.Error(err, "update api key, id: %d", id)
		return nil, errors.Default.Wrap(err, "error deleting api key")
	}
	apiKey.ApiKey = apiKeyStr
	return apiKey, nil
}

func (c *ApiKeyHelper) Delete(id uint64) errors.Error {
	// verify exists
	db := c.basicRes.GetDal()
	_, err := c.getApiKeyById(db, id)
	if err != nil {
		c.logger.Error(err, "get api key by id: %d", id)
		return err
	}
	err = db.Delete(&models.ApiKey{}, dal.Where("id = ?", id))
	if err != nil {
		c.logger.Error(err, "delete api key, id: %d", id)
		return errors.Default.Wrap(err, "error deleting api key")
	}
	return nil
}

func (c *ApiKeyHelper) DeleteForPlugin(tx dal.Transaction, pluginName string, extra string) errors.Error {
	// delete api key generated by plugin, for example webhook
	var apiKey models.ApiKey
	var clauses []dal.Clause
	if pluginName != "" {
		clauses = append(clauses, dal.Where("type = ?", fmt.Sprintf("plugin:%s", pluginName)))
	}
	if extra != "" {
		clauses = append(clauses, dal.Where("extra = ?", extra))
	}
	if err := tx.First(&apiKey, clauses...); err != nil {
		c.logger.Error(err, "query api key record")
		// if api key doesn't exist, just return success
		if tx.IsErrorNotFound(err.Unwrap()) {
			return nil
		} else {
			return err
		}
	}
	if err := tx.Delete(apiKey); err != nil {
		c.logger.Error(err, "delete api key record")
		return err
	}
	return nil
}

func (c *ApiKeyHelper) getApiKeyById(tx dal.Dal, id uint64, additionalClauses ...dal.Clause) (*models.ApiKey, errors.Error) {
	if tx == nil {
		tx = c.basicRes.GetDal()
	}
	apiKey := &models.ApiKey{}
	err := tx.First(apiKey, append([]dal.Clause{dal.Where("id = ?", id)}, additionalClauses...)...)
	if err != nil {
		if tx.IsErrorNotFound(err) {
			return nil, errors.NotFound.Wrap(err, fmt.Sprintf("could not find api key id[%d] in DB", id))
		}
		return nil, errors.Default.Wrap(err, "error getting api key from DB")
	}
	return apiKey, nil
}

func (c *ApiKeyHelper) GetApiKey(tx dal.Dal, additionalClauses ...dal.Clause) (*models.ApiKey, errors.Error) {
	if tx == nil {
		tx = c.basicRes.GetDal()
	}
	apiKey := &models.ApiKey{}
	err := tx.First(apiKey, additionalClauses...)
	return apiKey, err
}

func (c *ApiKeyHelper) generateApiKey() (apiKey string, hashedApiKey string, err errors.Error) {
	apiKey, randomLetterErr := utils.RandLetterBytes(apiKeyLen)
	if randomLetterErr != nil {
		err = errors.Default.Wrap(randomLetterErr, "random letters")
		return
	}
	hashedApiKey, err = c.DigestToken(apiKey)
	return apiKey, hashedApiKey, err
}

func (c *ApiKeyHelper) DigestToken(token string) (string, errors.Error) {
	h := hmac.New(sha256.New, []byte(c.encryptionSecret))
	if _, err := h.Write([]byte(token)); err != nil {
		c.logger.Error(err, "hmac write api key")
		return "", errors.Default.Wrap(err, "hmac write token")
	}
	hashedApiKey := fmt.Sprintf("%x", h.Sum(nil))
	return hashedApiKey, nil
}