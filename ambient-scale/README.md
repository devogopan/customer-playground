## ambient-scale

Small scale/resource experiments for **Istio ambient + sidecar (hybrid)**, focused on **ztunnel** and **waypoint** resource utilization.

## What we measured (final)

- **Ztunnel resource utilization vs traffic increase**
  - Each additional namespace adds **2 RPS** to total traffic in the cluster.

- **Waypoint resource utilization vs number of services (no traffic)**
  - Services are a mix of **ambient + sidecar**, mostly scaled in similar numbers.
  - There is **no traffic** in the cluster to eliminate the traffic angle.

- **Waypoint resource utilization vs traffic increase**
  - Only **one ambient namespace** generates traffic.
  - RPS is increased gradually.
  - Scaling was limited to keep **app/waypoint replicas at 1**.

## Results

See `metrics/` for collected outputs (CSV/PNG).

