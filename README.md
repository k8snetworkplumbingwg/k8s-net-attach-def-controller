# k8s-net-attach-def-controller

## Quickstart

Run below commands to quiclky download, build and run the controller:
```
go get https://github.com/K8sNetworkPlumbingWG/k8s-net-attach-def-controller
cd $GOPATH/src/github.com/K8sNetworkPlumbingWG/k8s-net-attach-def-controller
go build -o k8s-net-attach-def-controller
./k8s-net-attach-def-controller -kubeconfig=$HOME/.kube/config
```

## Example of net-attach-def deletion handling

Note: Network attachment definition CRD needs to be already created in the cluster and k8s-net-attach-def-controller has to be up and running.

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
