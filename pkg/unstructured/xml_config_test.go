/*
Copyright (C) 2022-2025 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package unstructured

import (
	"testing"

	"github.com/stretchr/testify/assert"

	parametersv1alpha1 "github.com/apecloud/kubeblocks/apis/parameters/v1alpha1"
)

func TestXMLFormat(t *testing.T) {
	const xmlContext = `
<!-- Settings profiles -->
<profiles>
    <!-- Default settings -->
    <default>
        <!-- The maximum number of threads when running a single query. -->
        <max_threads>8</max_threads>
    </default>

    <!-- Settings for quries from the user interface -->
    <web>
        <max_execution_time>600</max_execution_time>
        <min_execution_speed>1000000</min_execution_speed>
        <timeout_before_checking_execution_speed>15</timeout_before_checking_execution_speed>

        <readonly>1</readonly>
    </web>
</profiles>
`
	xmlConfigObj, err := LoadConfig("xml_test", xmlContext, parametersv1alpha1.XML)
	assert.Nil(t, err)

	assert.EqualValues(t, xmlConfigObj.Get("profiles.default.max_threads"), 8)
	assert.EqualValues(t, xmlConfigObj.Get("profiles.web.min_execution_speed"), 1000000)
	assert.EqualValues(t, xmlConfigObj.Get("profiles.web.max_threads"), nil)

	v, _ := xmlConfigObj.GetString("profiles.default.max_threads")
	assert.EqualValues(t, v, "8")
	v, _ = xmlConfigObj.GetString("profiles.web.min_execution_speed")
	assert.EqualValues(t, v, "1000000")

	_, err = xmlConfigObj.GetString("profiles.web.xxxx")
	assert.Nil(t, err)

	dumpContext, err := xmlConfigObj.Marshal()
	assert.Nil(t, err)
	newObj, err := LoadConfig("xml_test", dumpContext, parametersv1alpha1.XML)
	assert.Nil(t, err)
	assert.EqualValues(t, newObj.GetAllParameters(), xmlConfigObj.GetAllParameters())

	assert.Nil(t, xmlConfigObj.Update("profiles.web.timeout_before_checking_execution_speed", 200))
	assert.EqualValues(t, xmlConfigObj.Get("profiles.web.timeout_before_checking_execution_speed"), 200)

	assert.Nil(t, xmlConfigObj.RemoveKey("profiles.web.timeout_before_checking_execution_speed.sksds"))
	assert.Nil(t, xmlConfigObj.RemoveKey("profiles.web.timeout_before_checking_execution_speed"))
	assert.EqualValues(t, xmlConfigObj.Get("profiles.web.timeout_before_checking_execution_speed"), nil)

	assert.Nil(t, xmlConfigObj.Update("profiles.web2.timeout_before_checking_execution_speed", 600))
	assert.EqualValues(t, xmlConfigObj.Get("profiles.web2.timeout_before_checking_execution_speed"), 600)

	assert.Nil(t, xmlConfigObj.Update("profiles_test", "not_exist"))
	assert.EqualValues(t, xmlConfigObj.Get("profiles_test"), "not_exist")
}

func TestEmptyXMLFormat(t *testing.T) {
	xmlConfigObj, err := LoadConfig("empty_test", "", parametersv1alpha1.XML)
	assert.Nil(t, err)
	assert.NotNil(t, xmlConfigObj)
	assert.EqualValues(t, xmlConfigObj.GetAllParameters(), map[string]interface{}{})
	assert.Nil(t, xmlConfigObj.Update("profiles.web2.timeout_before_checking_execution_speed", 600))
	assert.EqualValues(t, xmlConfigObj.Get("profiles.web2.timeout_before_checking_execution_speed"), 600)

	// check None-EOF error
	_, err = LoadConfig("invalid test", "invalid xml format", parametersv1alpha1.XML)
	assert.NotNil(t, err)
}
