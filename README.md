# k8s-net-attach-def-controller

Custom controller adding support for using pods' secondary network interfaces as service endpoints and recovering deleted net-attach-defs objects which are still in use.

## Quickstart

Execute below commands to build and run the controller:
```
make image
kubectl apply -f deployments/rbac.yaml
kubectl apply -f deployments/k8s-net-attach-def-controller.yaml
```

## Examples

Note: Network attachment definition CRD needs to be already created in the cluster and k8s-net-attach-def-controller has to be up and running.

### Using pod secondary network interface as service endpoint

Create `br0` net-attach-def:
```
cat <<EOF | kubectl create -f -
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: br0
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.0",
      "type": "bridge",
      "bridge": "br0",
      "isGateway": true,
      "ipMasq": false,
      "hairpinMode": true,
      "mtu": 1450,
      "ipam": {
        "type": "host-local",
        "subnet": "192.168.0.0/24",
        "routes": [{
          "dst": "192.168.0.0/16"
        }]
      }
    }
EOF
```

Deploy `hostnames` application:
```
cat <<EOF | kubectl create -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hostnames
spec:
  selector:
    matchLabels:
      app: hostnames
  replicas: 1
  template:
    metadata:
      labels:
        app: hostnames
      annotations:
        k8s.v1.cni.cncf.io/networks: default/br0
    spec:
      containers:
      - name: hostnames
        image: k8s.gcr.io/serve_hostname
        ports:
        - containerPort: 9376
          protocol: TCP
EOF
```

Create `hostname` service with network annotation pointing to the `default/br0` network:
```
cat <<EOF | kubectl create -f -
kind: Service
apiVersion: v1
metadata:
  name: hostnames
  annotations:
    k8s.v1.cni.cncf.io/networks: br0
spec:
  selector:
    app: hostnames
  ports:
  - protocol: TCP
    port: 80
    targetPort: 9376
EOF
```

After a couple of seconds `hostnames` endpoints should be updated with address for secondary interface of the `hostnames-xyz-xyz` pod. To verify this run:
```
kubectl describe ep hostnames
```
In the `Subsets` section address from the 192.168.0.0/24 network should show up, for example:
```
Subsets:
  Addresses:          192.168.0.51
```
Events section should be updated with message coming from the k8s-net-attach-def-controller:
```
Events:
  Type    Reason                      Age   From                           Message
  ----    ------                      ----  ----                           -------
  Normal  Updated to use network br0  2s    k8s-net-attach-def-controller  Endpoints update succesful
```

Events section in the `hostnames` service should show the same message as well:
```
kubectl describe service hostnames
```
```
Name:              hostnames
Namespace:         default
...
Events:
  Type    Reason                      Age   From                           Message
  ----    ------                      ----  ----                           -------
  Normal  Updated to use network br0  15s   k8s-net-attach-def-controller  Endpoints update succesful
```

### Recovering net-attach-defs which are still in use

Create `foo-network` net-attach-def:
```
cat <<EOF | kubectl create -f -
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: foo-network
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.0",
      "name": "foo-network",
      "type": "bridge",
      "bridge": "br0",
      "isGateway": true,
      "ipam":
      {
        "type": "host-local",
        "subnet": "172.36.0.0/24",
        "dataDir": "/mnt/cluster-ipam"
      }
    }
EOF
```

Create a pod that uses `default/foo-network` by executing below command:
```
cat <<EOF | kubectl create -f -
apiVersion: v1
kind: Pod
metadata:
  name: demo
  annotations:
    k8s.v1.cni.cncf.io/networks: foo-network, foo-network
spec:
  containers:
  - image: busybox
    resources:
    command: ["tail", "-f", "/dev/null"]
    imagePullPolicy: IfNotPresent
    name: busybox
  restartPolicy: Always
EOF
```

Regardless of whether it has been succesfully created or not, try to remove `foo-network` net-attach-def with below command:
```
kubectl delete network-attachment-definitions.k8s.cni.cncf.io foo-network
```
Expected output is `networkattachmentdefinition.k8s.cni.cncf.io "foo-network" deleted`, however, after inspecting controller logs, you should notice output similiar to this:
```
2019/01/31 12:54:36 net-attach-def delete event received
2019/01/31 12:54:36 handling deletion of default/foo-network
2019/01/31 12:54:36 'foo-network, foo-network' is not in JSON format: invalid character 'o' in literal false (expecting 'a')... trying toparse as comma separated network selections list
2019/01/31 12:54:36 pod demo uses net-attach-def default/foo-network which needs to be recreated
2019/01/31 12:54:36 net-attach-def recovered: &{{ } {foo-network  default /apis/k8s.cni.cncf.io/v1/namespaces/default/network-attachment-definitions/foo-network 5ac43a1e-2557-11e9-83a9-feffe01e3c01  1 2019-01-31 12:54:32 +0000 GMT <nil> <nil> map[] map[] [] nil [] } {{
  "cniVersion": "0.3.0",
  "name": "foo-network",
  "type": "bridge",
  "bridge": "br0",
  "isGateway": true,
  "ipam":
  {
    "type": "host-local",
    "subnet": "172.36.0.0/24",
    "dataDir": "/mnt/cluster-ipam"
  }
}
}}
```
This means that `foo-network` is still in use, and because of that it has been "resurrected". To verify that run:
```
kubectl get network-attachment-definitions.k8s.cni.cncf.io foo-network
```
The output should show that this object has indeed been just recreated.
```
NAME          AGE
foo-network   2m
```

To do a cleanup, delete first pod and then the net-attach-def:
```
kubectl delete pod
kubectl delete network-attachment-definitions.k8s.cni.cncf.io foo-network
```
When executed in this order, both objects will be succesfully removed.
