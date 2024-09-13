// Code generated by mockery v2.45.0. DO NOT EDIT.

package mocks

import (
	v1alpha1 "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	mock "github.com/stretchr/testify/mock"
)

// HostManager is an autogenerated mock type for the HostManager type
type HostManager struct {
	mock.Mock
}

// DiscoverNicDevices provides a mock function with given fields:
func (_m *HostManager) DiscoverNicDevices() (map[string]v1alpha1.NicDeviceStatus, error) {
	ret := _m.Called()

	if len(ret) == 0 {
		panic("no return value specified for DiscoverNicDevices")
	}

	var r0 map[string]v1alpha1.NicDeviceStatus
	var r1 error
	if rf, ok := ret.Get(0).(func() (map[string]v1alpha1.NicDeviceStatus, error)); ok {
		return rf()
	}
	if rf, ok := ret.Get(0).(func() map[string]v1alpha1.NicDeviceStatus); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(map[string]v1alpha1.NicDeviceStatus)
		}
	}

	if rf, ok := ret.Get(1).(func() error); ok {
		r1 = rf()
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// NewHostManager creates a new instance of HostManager. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewHostManager(t interface {
	mock.TestingT
	Cleanup(func())
}) *HostManager {
	mock := &HostManager{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}