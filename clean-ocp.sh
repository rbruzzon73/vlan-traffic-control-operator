oc delete catalogsources.operators.coreos.com -n openshift-marketplace vlan-traffic-control-catalog
oc delete subscriptions vlan-traffic-control -n openshift-vlan-tc-operator
oc delete csv vlan-traffic-control.v${1} -n openshift-vlan-tc-operator

for node in $(oc get nodes -l node-role.kubernetes.io/worker -o jsonpath='{.items[*].metadata.name}'); do
  oc debug node/$node -- chroot /host tc qdisc del dev enp1s0 root 2>/dev/null
  oc debug node/$node -- chroot /host tc qdisc del dev enp1s0 ingress 2>/dev/null
done

oc delete project openshift-vlan-tc-operator -n openshift-vlan-tc-operator
