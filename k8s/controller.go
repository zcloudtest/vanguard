package k8s

import (
	"context"

	"github.com/zdnscloud/g53"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/zdnscloud/gok8s/cache"
	"github.com/zdnscloud/gok8s/client/config"
	"github.com/zdnscloud/gok8s/controller"
	"github.com/zdnscloud/gok8s/event"
	"github.com/zdnscloud/gok8s/handler"
	"github.com/zdnscloud/gok8s/predicate"
)

const (
	serviceIPIndex   = "service_with_ip"
	epNamespaceIndex = "endpoint_in_namespace"
)

type Controller struct {
	cache      cache.Cache
	controller controller.Controller
	auth       *Auth
	stopCh     chan struct{}
}

func NewK8sController(auth *Auth) (*Controller, error) {
	k8sCfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	cache, err := cache.New(k8sCfg, cache.Options{})
	if err != nil {
		return nil, err
	}
	cache.IndexField(&corev1.Service{}, serviceIPIndex, func(obj runtime.Object) []string {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return nil
		} else {
			return []string{svc.Spec.ClusterIP}
		}
	})

	cache.IndexField(&corev1.Endpoints{}, epNamespaceIndex, func(obj runtime.Object) []string {
		ep, ok := obj.(*corev1.Endpoints)
		if !ok {
			return nil
		}
		return []string{ep.ObjectMeta.Name + "." + ep.ObjectMeta.Namespace}
	})

	stopCh := make(chan struct{})
	go cache.Start(stopCh)
	cache.WaitForCacheSync(stopCh)

	controller := controller.New("vanguard_k8s_controller", cache, scheme.Scheme)
	controller.Watch(&corev1.Endpoints{})
	controller.Watch(&corev1.Service{})
	c := &Controller{
		controller: controller,
		cache:      cache,
		auth:       auth,
		stopCh:     stopCh,
	}
	go controller.Start(stopCh, c, predicate.NewIgnoreUnchangedUpdate())

	return c, nil
}

func (c *Controller) OnCreate(e event.CreateEvent) (handler.Result, error) {
	switch o := e.Object.(type) {
	case *corev1.Endpoints:
		c.handleEndPointCreate(o)
	case *corev1.Service:
		c.handleServiceCreate(o)
	}

	return handler.Result{}, nil
}

func (c *Controller) OnUpdate(e event.UpdateEvent) (handler.Result, error) {
	switch old := e.ObjectOld.(type) {
	case *corev1.Endpoints:
		new := e.ObjectNew.(*corev1.Endpoints)
		if len(old.Subsets) != 0 || len(new.Subsets) != 0 {
			s, err := c.getService(old.Name, old.Namespace)
			if err == nil {
				c.handleEndPointUpdate(s, old, new)
			}
		}
	case *corev1.Service:
		new := e.ObjectNew.(*corev1.Service)
		c.handleServiceUpdate(old, new)
	}
	return handler.Result{}, nil
}

func (c *Controller) OnDelete(e event.DeleteEvent) (handler.Result, error) {
	switch o := e.Object.(type) {
	case *corev1.Endpoints:
		c.handleEndPointDelete(o)
	case *corev1.Service:
		c.handleServiceDelete(o)
	}
	return handler.Result{}, nil
}

func (c *Controller) OnGeneric(e event.GenericEvent) (handler.Result, error) {
	return handler.Result{}, nil
}

func (c *Controller) getService(name, namespace string) (*corev1.Service, error) {
	var service corev1.Service
	err := c.cache.Get(context.TODO(), types.NamespacedName{namespace, name}, &service)
	if err == nil {
		return &service, nil
	} else {
		return nil, err
	}
}

func (c *Controller) handleServiceCreate(svc *corev1.Service) {
	if isNormalService(svc) {
		c.addServiceRecord(svc)
	} else if isExternalService(svc) {
		c.addExternalServiceRecord(svc)
	}
}

func (c *Controller) handleServiceDelete(svc *corev1.Service) {
	if isNormalService(svc) {
		c.deleteServiceRecord(svc)
	} else if isHeaderlessService(svc) {
		c.deleteHeadlessServiceRecord(svc)
	} else if isExternalService(svc) {
		c.deleteExternalServiceRecord(svc)
	}
}

func (c *Controller) handleServiceUpdate(old, new *corev1.Service) {
	if isNormalService(old) {
		if old.Spec.ClusterIP != new.Spec.ClusterIP {
			c.deleteServiceRecord(old)
			c.addServiceRecord(new)
		}
	} else if isExternalService(new) {
		if old.Spec.ExternalName != new.Spec.ExternalName {
			c.addServiceRecord(new)
		}
	}
}

func (c *Controller) handleEndPointCreate(o *corev1.Endpoints) {
	svc, err := c.getService(o.Name, o.Namespace)
	if err == nil {
		c.addPodRecord(svc, o)
		if isHeaderlessService(svc) {
			c.addHeadlessServiceRecord(svc, o)
		}
	}
}

func (c *Controller) handleEndPointUpdate(svc *corev1.Service, old, new *corev1.Endpoints) {
	if isSubsetsEqual(old, new) {
		return
	}

	//header less service rrset is a list of pods
	if isHeaderlessService(svc) {
		c.addHeadlessServiceRecord(svc, new)
	}

	c.deletePodRecord(old)
	c.addPodRecord(svc, new)
}

func (c *Controller) handleEndPointDelete(o *corev1.Endpoints) {
	c.deletePodRecord(o)
}

func (c *Controller) addPodRecord(svc *corev1.Service, o *corev1.Endpoints) {
	for _, subset := range o.Subsets {
		var podNames []*g53.Name
		var addrs [][]string
		//pod may has same name when hostname and subdomain is same :(
		for _, addr := range subset.Addresses {
			n := c.auth.getEndpointsAddrDomain(&addr, o.Name, o.Namespace)
			duplicateName := false
			duplicateNameIndex := 0
			for i, n_ := range podNames {
				if n_.Equals(n) {
					duplicateNameIndex = i
					duplicateName = true
					break
				}
			}
			if duplicateName {
				addrs[duplicateNameIndex] = append(addrs[duplicateNameIndex], addr.IP)
			} else {
				podNames = append(podNames, n)
				addrs = append(addrs, []string{addr.IP})
			}
		}

		for i, n := range podNames {
			a := &g53.RRset{
				Name:  n,
				Type:  g53.RR_A,
				Class: g53.CLASS_IN,
				Ttl:   defaultTTL,
			}
			var rdatas []g53.Rdata
			for _, ip := range addrs[i] {
				rdata, _ := g53.AFromString(ip)
				rdatas = append(rdatas, rdata)
				if rn, err := c.auth.getReverseName(ip); err == nil {
					ptr := &g53.RRset{
						Name:   rn,
						Type:   g53.RR_PTR,
						Class:  g53.CLASS_IN,
						Ttl:    defaultTTL,
						Rdatas: []g53.Rdata{&g53.PTR{Name: n}},
					}
					c.auth.replacePodReverseRRset(rn, g53.RR_PTR, ptr)
				}
			}
			a.Rdatas = rdatas
			c.auth.replaceServiceRRset(n, g53.RR_A, a)
		}

		for _, port := range subset.Ports {
			if port.Name == "" {
				continue
			}

			n := c.auth.getPortName(port.Name, string(port.Protocol), o.Name, o.Namespace)
			srv := &g53.RRset{
				Name:  n,
				Type:  g53.RR_SRV,
				Class: g53.CLASS_IN,
				Ttl:   defaultTTL,
			}
			var rdatas []g53.Rdata
			if isHeaderlessService(svc) {
				for _, podName := range podNames {
					rdatas = append(rdatas, &g53.SRV{
						Priority: defaultPriority,
						Weight:   defaultWeight,
						Port:     uint16(port.Port),
						Target:   podName,
					})
				}
			} else {
				rdatas = append(rdatas, &g53.SRV{
					Priority: defaultPriority,
					Weight:   defaultWeight,
					Port:     uint16(port.Port),
					Target:   c.auth.getServiceDomain(svc),
				})
			}
			srv.Rdatas = rdatas
			c.auth.replaceServiceRRset(n, g53.RR_SRV, srv)
		}
	}
}

func (c *Controller) deletePodRecord(o *corev1.Endpoints) {
	for _, subset := range o.Subsets {
		for _, addr := range subset.Addresses {
			n := c.auth.getEndpointsAddrDomain(&addr, o.Name, o.Namespace)
			c.auth.replaceServiceRRset(n, g53.RR_A, nil)
			if rn, err := c.auth.getReverseName(addr.IP); err == nil {
				c.auth.replacePodReverseRRset(rn, g53.RR_PTR, nil)
			}
		}

		for _, port := range subset.Ports {
			if port.Name != "" {
				n := c.auth.getPortName(port.Name, string(port.Protocol), o.Name, o.Namespace)
				c.auth.replaceServiceRRset(n, g53.RR_SRV, nil)
			}
		}
	}
}

func (c *Controller) addHeadlessServiceRecord(svc *corev1.Service, ep *corev1.Endpoints) {
	//handle a rrset for service domain
	rdatas := []g53.Rdata{}
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			if addr.IP != "" {
				rdata, _ := g53.AFromString(addr.IP)
				rdatas = append(rdatas, rdata)
			}
		}
	}
	if len(rdatas) != 0 {
		n := c.auth.getServiceDomain(svc)
		a := &g53.RRset{
			Name:   n,
			Type:   g53.RR_A,
			Class:  g53.CLASS_IN,
			Ttl:    defaultTTL,
			Rdatas: rdatas,
		}
		c.auth.replaceServiceRRset(n, g53.RR_A, a)
	}
}

func (c *Controller) addExternalServiceRecord(svc *corev1.Service) {
	n := c.auth.getServiceDomain(svc)
	en, err := g53.NameFromString(svc.Spec.ExternalName)
	if err == nil {
		cname := &g53.RRset{
			Name:   n,
			Type:   g53.RR_CNAME,
			Class:  g53.CLASS_IN,
			Ttl:    defaultTTL,
			Rdatas: []g53.Rdata{&g53.CName{Name: en}},
		}
		c.auth.replaceServiceRRset(n, g53.RR_CNAME, cname)
	}
}

func (c *Controller) addServiceRecord(svc *corev1.Service) {
	n := c.auth.getServiceDomain(svc)
	rdata, _ := g53.AFromString(svc.Spec.ClusterIP)
	a := &g53.RRset{
		Name:   n,
		Type:   g53.RR_A,
		Class:  g53.CLASS_IN,
		Ttl:    defaultTTL,
		Rdatas: []g53.Rdata{rdata},
	}
	c.auth.replaceServiceRRset(n, g53.RR_A, a)

	rn, err := c.auth.getReverseName(svc.Spec.ClusterIP)
	if err == nil {
		ptr := &g53.RRset{
			Name:   rn,
			Type:   g53.RR_PTR,
			Class:  g53.CLASS_IN,
			Ttl:    defaultTTL,
			Rdatas: []g53.Rdata{&g53.PTR{Name: n}},
		}
		c.auth.replaceServiceReverseRRset(rn, g53.RR_PTR, ptr)
	}
}

func (c *Controller) deleteServiceRecord(svc *corev1.Service) {
	n := c.auth.getServiceDomain(svc)
	c.auth.replaceServiceRRset(n, g53.RR_A, nil)
	if rn, err := c.auth.getReverseName(svc.Spec.ClusterIP); err == nil {
		c.auth.replaceServiceReverseRRset(rn, g53.RR_PTR, nil)
	}
}

func (c *Controller) deleteExternalServiceRecord(svc *corev1.Service) {
	n := c.auth.getServiceDomain(svc)
	c.auth.replaceServiceRRset(n, g53.RR_CNAME, nil)
}

func (c *Controller) deleteHeadlessServiceRecord(svc *corev1.Service) {
	n := c.auth.getServiceDomain(svc)
	c.auth.replaceServiceRRset(n, g53.RR_A, nil)
}
