## 第25章 Operator（重点）

> Operator = CRD + 控制循环。`controller-runtime` 把 Kubernetes 声明式 API 的“期望状态 → 实际状态”收敛逻辑封装成一套可复用的工程框架，让你专注于 Reconcile，而不用自己拼装 List/Watch/队列/Leader Election/Webhook 等基础设施。

### controller-runtime

`controller-runtime` 是 kubebuilder / Operator SDK 背后的核心库。它在 [第23章 client-go](./23-client-go.md) 的 Informer 体系和 [第24章 Controller](./24-Controller.md) 的 WorkQueue + Reconcile 模式之上，提供更高层的抽象。

**为什么需要它**：用裸 client-go 写一个控制器要手动接线 Reflector → DeltaFIFO → Indexer → workqueue → Reconcile，还要自己处理 Leader Election、Metrics、Webhook、优雅退出。`controller-runtime` 把这些全部收口到 `Manager` 里，开发者只需实现 `Reconcile(request)` 一个方法。

核心包：

| 包 | 作用 |
|---|---|
| `pkg/manager` | Manager：根组件，持有 Cache/Client/Controller/Webhook |
| `pkg/client` | Client：读缓存、写 apiserver 的分裂客户端 |
| `pkg/cache` | Cache：基于 Informer 的本地对象缓存 |
| `pkg/reconcile` | Reconcile 接口 |
| `pkg/controller` | Controller 构建器（builder） |
| `pkg/webhook` | 准入 Webhook 服务 |
| `pkg/manager` (信号) | 优雅启停 |

整体架构：

```
        ┌──────────────────── Manager ─────────────────────┐
        │  Cache(Informer)   Client   WebhookServer        │
        │  Healthz/Metrics/LeaderElection                  │
        │        │             │                            │
        │     Watch 事件    Get/Update/Patch                │
        │        ▼             ▼                            │
        │   ┌────── Controller(Reconcile) ──────┐          │
        │   │ WorkQueue → Reconcile(req)        │          │
        │   │            → Client.Get/Update    │          │
        │   └───────────────────────────────────┘          │
        └──────────────────────────────────────────────────┘
```

最小骨架：

```go
package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	ctrl.SetLogger(zap.New())
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics: metricsserver.Options{BindAddress: ":8080"},
	})
	if err != nil {
		os.Exit(1)
	}
	// 注册 Reconciler ...
	_ = mgr.Start(ctrl.SetupSignalHandler())
}
```

### Manager

`Manager` 是根组件，负责持有并生命周期管理 Cache、Client、所有 Controller、Webhook Server、Metrics/Healthz/PProf 服务，以及 Leader Election 和优雅退出。

`ctrl.Options` 关键字段：

| 字段 | 说明 |
|---|---|
| `Metrics` | Metrics server 监听地址（Prometheus 抓取） |
| `HealthProbeBindAddress` | healthz/readyz 探针地址 |
| `LeaderElection` | 是否开启 Leader Election |
| `LeaderElectionID` | Lease 名，集群内全局唯一 |
| `Cache` | Cache 配置（选择性缓存） |
| `WebhookServer` | Webhook TLS server |

```go
mgr, err := ctrl.NewManager(cfg, ctrl.Options{
	Scheme:                 scheme,
	Metrics:                metricsserver.Options{BindAddress: ":8080"},
	HealthProbeBindAddress: ":8081",
	LeaderElection:         true,
	LeaderElectionID:       "my-operator.example.com",
})
```

**Leader Election**：基于 `coordination.k8s.io/Lease`。多副本部署时，只有持有 Lease 的副本真正执行 Reconcile，其余待命；Leader 宕机后 Lease 过期，其他副本接管。这避免了多副本同时写对象造成冲突，是生产 Operator 的必备项。

> 坑：`LeaderElectionID` 在集群内必须唯一，否则两个 Operator 会抢同一个 Lease 互相踢。

### Cache

`Cache` 是对 `SharedInformer`（见 [第23章 client-go](./23-client-go.md)）的封装。它在本地维护一份 Watch 到的对象副本，使 `Client.Get/List` 直接读内存（无 apiserver 往返），并通过 List+Watch 接收变更事件驱动 Reconcile。

**关键设计**：

- Cache 与 apiserver 是**最终一致**的：`Get` 读到的是上一次 Watch 同步的快照，可能比 apiserver 稍滞后几毫秒。
- 默认缓存该 Controller Watch 的所有 GVK 的所有命名空间对象。大集群下会吃大量内存，可用 `cache.Options.ByObject` 做**选择性缓存**：按 namespace、label/field selector 限制。

```go
import (
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	appsv1 "k8s.io/api/apps/v1"
)

mgr, _ := ctrl.NewManager(cfg, ctrl.Options{
	Cache: cache.Options{
		ByObject: map[client.Object]cache.Object{
			&appsv1.Deployment{}: {
				Label: labels.SelectorFromSet(labels.Set{"app":"watched"}),
			},
		},
	},
})
```

> 坑：若某 GVK 被设置成“不缓存”（`DisableFor`），则 `Client.Get` 会回退为直接打 apiserver，慢且压力增大。一般只缓存真正驱动的对象。

### Client

`Client` 是**分裂客户端（split client）**：读（Get/List）走 Cache，写（Create/Update/Patch/Delete）直接打 apiserver。它实现 `client.Client` 接口。

```go
type Client interface {
	Get(ctx, key, obj) error
	List(ctx, list, opts...) error
	Create(ctx, obj, opts...) error
	Update(ctx, obj, opts...) error
	Patch(ctx, obj, patch, opts...) error
	Delete(ctx, obj, opts...) error
	DeleteAllOf(ctx, obj, opts...) error
	Status() SubResourceWriter
	SubResource(sub string) SubResourceClient
}
```

**Status 子资源**：Kubernetes 中 `.spec` 和 `.status` 是分开持久化的。更新 `.status` 必须用 `r.Client.Status().Update(ctx, obj)`，否则会被 apiserver 拒绝或覆盖 `.spec`。

**冲突与重试**：高并发下 `Update` 常因 `optimisticLock`（基于 `resourceVersion`）冲突。用 `retry.RetryOnConflict` 包裹：

```go
import "k8s.io/client-go/util/retry"

_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
	if err := r.Get(ctx, req.NamespacedName, &appsv1.Deployment{}); err != nil {
		return err
	}
	// 修改 ...
	return r.Update(ctx, &dep)
})
```

**Server-Side Apply (SSA)**：推荐用 `r.Client.Patch(ctx, obj, client.Apply, client.ForceOwnership)`，让 apiserver 做 field 级别 ownership 合并，避免整对象覆盖冲突。

### Webhook

Webhook 是**准入控制器（Admission Webhook）**：在对象持久化前由 apiserver 调用你的服务，分为 Mutating（可改对象）和 Validating（只能放行/拒绝）。controller-runtime 用 `webhook.Admission` handler 实现，由 Manager 的 WebhookServer（默认 `:9443` TLS）暴露。

三种用途：

| 类型 | 作用 | kubebuilder 标记 |
|---|---|---|
| Defaulter | 设置默认值（mutating） | `+kubebuilder:webhook:mutating=true` |
| Validator | 校验 Create/Update/Delete | `+kubebuilder:webhook:mutating=false` |
| Conversion | 转换 CRD 多版本 | `+kubebuilder:conversion` |

最小 Defaulter + Validator 示例：

```go
package v1

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// +kubebuilder:webhook:path=/mutate-example-com-v1-myapp,mutating=true,failurePolicy=fail,groups=example.com,resources=myapps,verbs=create;update,versions=v1,name=mmyapp.kb.io

type MyAppDefaulter struct{}

func (d *MyAppDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	o := obj.(*MyApp)
	if o.Spec.Replicas == nil {
		replicas := int32(1)
		o.Spec.Replicas = &replicas
	}
	return nil
}

// 实现 webhook.CustomDefaulter 接口后注册到 Manager
// mgr.GetWebhookServer().Register("/mutate-...", webhook.CustomDefaulter...)
```

> 坑：Webhook 必须用 TLS，且证书需被 apiserver 信任（通过 `MutatingWebhookConfiguration.clientConfig.caBundle`）。kubebuilder 用 `cert-controller` 自动注入；手写时证书管理是头号坑源。

### 最小 CRD + Reconcile 完整示例

定义一个 `MyApp` CRD，Reconcile 根据 `Spec.Replicas` 维护一个同名 Deployment。

```go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MyAppSpec struct {
	Replicas *int32 `json:"replicas"`
	Image    string `json:"image"`
}

type MyAppStatus struct {
	Ready bool `json:"ready"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type MyApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   MyAppSpec   `json:"spec,omitempty"`
	Status MyAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MyAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MyApp `json:"items"`
}
```

Reconciler：

```go
package controllers

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	examplev1 "example.com/myapp/api/v1"
)

type MyAppReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=example.com,resources=myapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
func (r *MyAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var myapp examplev1.MyApp
	if err := r.Get(ctx, req.NamespacedName, &myapp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var dep appsv1.Deployment
	err := r.Get(ctx, req.NamespacedName, &dep)
	if errors.IsNotFound(err) {
		dep = appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: myapp.Name, Namespace: myapp.Namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: myapp.Spec.Replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": myapp.Name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": myapp.Name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: myapp.Spec.Image}}},
				},
			},
		}
		if err := controllerutil.SetControllerReference(&myapp, &dep, r.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &dep); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// 期望状态收敛：replicas / image 不一致就更新
	updated := false
	if dep.Spec.Replicas != myapp.Spec.Replicas {
		dep.Spec.Replicas = myapp.Spec.Replicas
		updated = true
	}
	if len(dep.Spec.Template.Spec.Containers) > 0 &&
		dep.Spec.Template.Spec.Containers[0].Image != myapp.Spec.Image {
		dep.Spec.Template.Spec.Containers[0].Image = myapp.Spec.Image
		updated = true
	}
	if updated {
		if err := r.Update(ctx, &dep); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 回写 status
	ready := dep.Status.ReadyReplicas == int32(pointerInt32(myapp.Spec.Replicas))
	meta.SetStatusCondition(&myapp.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: boolToStatus(ready), Reason: "Reconciled", ObservedGeneration: myapp.Generation,
	})
	if myapp.Status.Ready != ready {
		myapp.Status.Ready = ready
		_ = r.Status().Update(ctx, &myapp)
	}

	return ctrl.Result{}, nil
}

func (r *MyAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&examplev1.MyApp{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func pointerInt32(p *int32) int32 { if p == nil { return 0 }; return *p }
func boolToStatus(b bool) metav1.ConditionStatus { if b { return metav1.ConditionTrue }; return metav1.ConditionFalse }
```

> 关键点：`For(&MyApp{})` 注册主对象 Watch；`Owns(&Deployment{})` 让 Deployment 变化也触发 Reconcile（控制关系由 `SetControllerReference` 建立）。`Owns` 是 Operator 的精髓——子资源变化自动回到主循环。

### 本章小结

- `controller-runtime` = Manager + Cache + Client + Controller + Webhook 的工程化封装，核心是让你只写 `Reconcile`。
- Manager 管生命周期、Leader Election、Metrics/Healthz；生产部署必须开 Leader Election。
- Cache 是 Informer 本地缓存，读快但最终一致；大集群用 `ByObject` 选择性缓存省内存。
- Client 是分裂客户端：读缓存、写 apiserver；status 走 `Status().Update`；高并发用 `RetryOnConflict` 或 SSA Patch。
- Webhook 实现 Defaulter/Validator，必须 TLS + 受信证书。
- `For` + `Owns` + `SetControllerReference` 构成 CRD 控制关系的三角，是 Operator 的标准范式。
