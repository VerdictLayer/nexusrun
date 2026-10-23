#!/bin/sh
# Reads {"order":"1042"} on stdin and prints that order's status from the
# unit's order file. The token stands in for an order system's credential: the
# tool refuses to answer without it.
if [ -z "$ORDERS_TOKEN" ]; then
  echo "Error: ORDERS_TOKEN is not set on this host"
  exit 0
fi
order=$(tr -d '\n' | sed -n 's/.*"order"[[:space:]]*:[[:space:]]*"\{0,1\}\([A-Za-z0-9-]*\).*/\1/p')
line=$(grep "^$order|" "$(dirname "$0")/../data/orders.txt")
if [ -z "$order" ] || [ -z "$line" ]; then
  echo "No order numbered ${order:-(none)}"
  exit 0
fi
echo "Order $order: ${line#*|}"
