## 第31章 Operator（重点）

> 版本基线：`sigs.k8s.io/controller-runtime v0.24.1`，Kubernetes libraries `v0.36.0`，Go 1.26。Operator = API + 控制循环 + 运维契约，不只是 CRD 和一段 Reconcile。

### controller-runtime

`controller-runtime` 是 kubebuilder / Operator SDK 背后的核心库。它在 [第29章 client-go](./29-client-go.md) 的 Informer 体系和 [第30章 Controller](./30-Controller.md) 的 WorkQueue + Reconcile 模式之上，提供更高层的抽象。

**为什么需要它**：用裸 client-go 写控制器要手动接线 Reflector、Queue、Indexer、workqueue 和协调循环，还要处理 Leader Election、Metrics、Webhook 与优雅退出。`controller-runtime` 把这些组件收口到 `Manager`，让业务代码集中在 Reconcile 与 API/运维契约上。

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
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
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
| `LeaderElectionID` | Lease 名，在 leader-election namespace 内应避免非预期冲突 |
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

**Leader Election**：基于 `coordination.k8s.io/Lease`。开启后，多副本中只有持有 Lease 的实例启动需要选主的 Runnable。它适合不能并发执行的全局任务或减少重复外部副作用，但不是所有 Operator 的强制项，也不能替代幂等和乐观并发；有些控制器可安全 active-active。

> 坑：Lease 是 namespace-scoped。不同程序若在同一个 leader-election namespace 使用相同 `LeaderElectionID`，会被当成同一组选主参与者；除非这是有意设计，否则必须使用不同 ID。

### Cache

`Cache` 是对 `SharedInformer`（见 [第29章 client-go](./29-client-go.md)）的封装。它在本地维护一份 Watch 到的对象副本，使 `Client.Get/List` 直接读内存（无 apiserver 往返），并通过 List+Watch 接收变更事件驱动 Reconcile。

**关键设计**：

- Cache 与 apiserver 是**最终一致**的：`Get` 读到的是最近由 List/Watch 应用到本地的状态，滞后时间没有固定上限。
- Controller 声明的 Watch 会启动相应 Informer。默认情况下，对尚无 Informer 的类型执行缓存 `Get/List` 还可能按需创建 Informer 并等待同步；大集群可用 `ByObject`、`DefaultNamespaces` 和 selector 限制缓存，也可用 `ReaderFailOnMissingInformer` 把意外读取变成显式错误。

```go
import (
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

mgr, err := ctrl.NewManager(cfg, ctrl.Options{
	Cache: cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&appsv1.Deployment{}: {
				Label: labels.SelectorFromSet(labels.Set{"app": "watched"}),
			},
		},
	},
})
if err != nil {
	return err
}
```

> 坑：selector 之外的对象对这份 Cache 来说就是不存在，不会自动回退为 live read。若在 `ctrl.Options.Client.Cache.DisableFor` 中配置某类型，该类型的读才会始终直连 apiserver。两者语义不同，都要计入一致性和控制面容量设计。

### Client

`Client` 是**分裂客户端（split client）**：读（Get/List）走 Cache，写（Create/Update/Patch/Delete）直接打 apiserver。它实现 `client.Client` 接口。

```go
type Client interface {
	Get(ctx, key, obj) error
	List(ctx, list, opts...) error
	Apply(ctx, applyConfiguration, opts...) error
	Create(ctx, obj, opts...) error
	Update(ctx, obj, opts...) error
	Patch(ctx, obj, patch, opts...) error
	Delete(ctx, obj, opts...) error
	DeleteAllOf(ctx, obj, opts...) error
	Status() SubResourceWriter
	SubResource(sub string) SubResourceClient
}
```

上面省略了 `Scheme`、`RESTMapper`、GVK/作用域查询等辅助方法。Manager 生成的 Client 通常以 Cache 作为 Reader，以直连客户端作为 Writer；`DisableFor` 等配置会改变具体读路径。

**Status 子资源**：启用 status subresource 后，`.spec` 和 `.status` 分开写入。此时用 `Status().Update/Patch/Apply` 修改 status，并为 `<resource>/status` 配置 RBAC；普通 `Update` 不会替你持久化 status。

**冲突与重试**：`Update` 或带 optimistic lock 的 Patch 会基于 `resourceVersion` 检测冲突。Reconcile 通常直接返回冲突错误，让 workqueue 稍后以新缓存状态重试。若确实要在一次调用内使用 `RetryOnConflict`，每轮必须 live read；反复从可能滞后的 Cache 读取同一个 RV 只会重复冲突：

```go
import "k8s.io/client-go/util/retry"

if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
	var dep appsv1.Deployment
	if err := r.APIReader.Get(ctx, req.NamespacedName, &dep); err != nil {
		return err
	}
	// 基于 live read 得到的最新 resourceVersion 修改 dep。
	return r.Update(ctx, &dep)
}); err != nil {
	return err
}
```

这里的 `APIReader` 可由 `mgr.GetAPIReader()` 注入。重试必须受 `ctx` 和整体延迟预算约束；不要在 Reconcile 内再套无界重试。

**Server-Side Apply (SSA)**：使用稳定 `FieldOwner`，且 Apply 对象只携带本控制器拥有的字段。默认不要 `ForceOwnership`；它会夺取其他 field manager 的字段，只适合明确的所有权迁移。

### Webhook

Webhook 是**准入控制器（Admission Webhook）**：在对象持久化前由 apiserver 调用你的服务，分为 Mutating（可改对象）和 Validating（只能放行/拒绝）。controller-runtime 用 `admission.Webhook` 及泛型 Defaulter/Validator 封装，由 Manager 的 WebhookServer（默认 `:9443` TLS）暴露。

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
	"context"
	"errors"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/mutate-example-com-v1-myapp,mutating=true,failurePolicy=fail,sideEffects=None,groups=example.com,resources=myapps,verbs=create;update,versions=v1,name=mmyapp.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-example-com-v1-myapp,mutating=false,failurePolicy=fail,sideEffects=None,groups=example.com,resources=myapps,verbs=create;update,versions=v1,name=vmyapp.kb.io,admissionReviewVersions=v1

type MyAppDefaulter struct{}

func (d *MyAppDefaulter) Default(ctx context.Context, obj *MyApp) error {
	if obj.Spec.Replicas == nil {
		replicas := int32(1)
		obj.Spec.Replicas = &replicas
	}
	return nil
}

type MyAppValidator struct{}

func (*MyAppValidator) ValidateCreate(ctx context.Context, obj *MyApp) (admission.Warnings, error) {
	return nil, validateMyApp(obj)
}

func (*MyAppValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *MyApp) (admission.Warnings, error) {
	return nil, validateMyApp(newObj)
}

func (*MyAppValidator) ValidateDelete(ctx context.Context, obj *MyApp) (admission.Warnings, error) {
	return nil, nil
}

func validateMyApp(obj *MyApp) error {
	if obj.Spec.Image == "" {
		return errors.New("spec.image must not be empty")
	}
	return nil
}

func (r *MyApp) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &MyApp{}).
		WithDefaulter(&MyAppDefaulter{}).
		WithValidator(&MyAppValidator{}).
		Complete()
}
```

> 坑：Webhook 必须用 TLS，证书需被 apiserver 通过 `caBundle` 信任。Kubebuilder 生成部署清单和标记，但证书签发/轮换通常还要接入 cert-manager、平台证书控制器或自建流程；不能假设脚手架会自动完成生产证书管理。

### 最小 CRD + Reconcile 完整示例

定义一个 `MyApp` CRD，Reconcile 根据 `Spec.Replicas` 维护一个同名 Deployment。

```go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MyAppSpec struct {
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
}

type MyAppStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	examplev1 "example.com/myapp/api/v1"
)

type MyAppReconciler struct {
	client.Client
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=example.com,resources=myapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=example.com,resources=myapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
func (r *MyAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var myapp examplev1.MyApp
	if err := r.Get(ctx, req.NamespacedName, &myapp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !myapp.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	desiredReplicas := int32(1)
	if myapp.Spec.Replicas != nil {
		desiredReplicas = *myapp.Spec.Replicas
	}
	selectorLabels := map[string]string{"app.kubernetes.io/name": myapp.Name}

	var dep appsv1.Deployment
	err := r.Get(ctx, req.NamespacedName, &dep)
	if apierrors.IsNotFound(err) {
		dep = appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: myapp.Name, Namespace: myapp.Namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(desiredReplicas),
				Selector: &metav1.LabelSelector{MatchLabels: selectorLabels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": myapp.Name}},
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
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		if !metav1.IsControlledBy(&dep, &myapp) {
			return ctrl.Result{}, fmt.Errorf("deployment %s/%s is not controlled by MyApp UID %s", dep.Namespace, dep.Name, myapp.UID)
		}

		before := dep.DeepCopy()
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != desiredReplicas {
			dep.Spec.Replicas = ptr.To(desiredReplicas)
		}
		if dep.Spec.Template.Labels == nil {
			dep.Spec.Template.Labels = map[string]string{}
		}
		dep.Spec.Template.Labels["app.kubernetes.io/name"] = myapp.Name

		mainContainer := -1
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == "main" {
				mainContainer = i
				break
			}
		}
		if mainContainer == -1 {
			dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers,
				corev1.Container{Name: "main", Image: myapp.Spec.Image})
		} else {
			dep.Spec.Template.Spec.Containers[mainContainer].Image = myapp.Spec.Image
		}

		if !equality.Semantic.DeepEqual(before.Spec, dep.Spec) {
			patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
			if err := r.Patch(ctx, &dep, patch); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	statusBefore := myapp.DeepCopy()
	ready := dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.UpdatedReplicas == desiredReplicas &&
		dep.Status.AvailableReplicas == desiredReplicas
	conditionStatus := metav1.ConditionFalse
	reason := "DeploymentProgressing"
	message := "Deployment has not reached the desired state"
	if ready {
		conditionStatus = metav1.ConditionTrue
		reason = "DeploymentReady"
		message = "Deployment reached the desired state"
	}
	myapp.Status.ObservedGeneration = myapp.Generation
	myapp.Status.ReadyReplicas = dep.Status.ReadyReplicas
	meta.SetStatusCondition(&myapp.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: myapp.Generation,
	})
	if !equality.Semantic.DeepEqual(statusBefore.Status, myapp.Status) {
		patch := client.MergeFromWithOptions(statusBefore, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, &myapp, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *MyAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&examplev1.MyApp{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
```

> 关键点：`For(&MyApp{})` 注册主对象 Watch；`Owns(&Deployment{})` 根据 controller owner reference 把 Deployment 变化映射回 MyApp。Create/Patch 成功后通常依靠 Watch 再次协调，不需要返回已弃用的 `Result.Requeue`；需要定时轮询外部系统时才使用 `RequeueAfter`。

### 本章小结

- `controller-runtime` = Manager + Cache + Client + Controller + Webhook 的工程化封装，核心是让你只写 `Reconcile`。
- Manager 管生命周期、Leader Election、Metrics/Healthz；是否选主取决于副作用和吞吐模型，控制器本身始终要幂等。
- Cache 是 Informer 本地缓存，读快但最终一致；大集群用 `ByObject` 选择性缓存省内存。
- Client 通常读 Cache、写 apiserver；status 走 `Status()` 子资源客户端。冲突可交给 workqueue 重试，必须内联重试时每轮使用 live read；字段协作优先评估 SSA。
- Webhook 实现 Defaulter/Validator，必须 TLS + 受信证书。
- `For` + `Owns` + `SetControllerReference` 构成 CRD 控制关系的三角，是 Operator 的标准范式。
