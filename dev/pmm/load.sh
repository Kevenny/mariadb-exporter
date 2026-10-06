#!/bin/bash
# Light, continuous workload against the dev MariaDB.
q() { mariadb -h mariadb -uroot -proot demo -e "$1" 2>/dev/null; }

q "CREATE TABLE IF NOT EXISTS orders (
     id INT AUTO_INCREMENT PRIMARY KEY,
     customer INT NOT NULL,
     amount DECIMAL(10,2) NOT NULL,
     created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
     KEY idx_customer (customer),
     KEY idx_unused (amount, created_at))"

while true; do
  q "INSERT INTO orders (customer, amount)
     SELECT FLOOR(RAND()*1000), RAND()*500 FROM seq_1_to_50"
  q "SELECT customer, SUM(amount) FROM orders WHERE customer = FLOOR(RAND()*1000) GROUP BY customer" >/dev/null
  q "SELECT COUNT(*) FROM orders WHERE amount > 400" >/dev/null
  q "UPDATE orders SET amount = amount + 1 WHERE customer = FLOOR(RAND()*1000)"
  q "DELETE FROM orders ORDER BY id LIMIT 40"
  sleep 0.2
done
