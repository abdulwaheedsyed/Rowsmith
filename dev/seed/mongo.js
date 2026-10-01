// Sample "shop" database for MongoDB. dev/compose.yaml loads it on first start;
// to reload it: mongosh -u root -p <pw> dev/seed/mongo.js
db = db.getSiblingDB("shop");
db.dropDatabase();
const names = ["Ava", "Liam", "Mia", "Noah", "Zoe", "Omar", "Lena", "Kai", "Ines", "Yuki", "Ravi", "Sofia"];
const cities = [["Amsterdam", 4.8952, 52.3702], ["Rotterdam", 4.4777, 51.9244], ["Utrecht", 5.1214, 52.0907], ["Lisbon", -9.1393, 38.7223], ["Osaka", 135.5023, 34.6937]];
const customers = [];
for (let i = 0; i < 2000; i++) {
  const c = cities[i % cities.length];
  customers.push({
    email: `user${i}@example.com`,
    name: `${names[i % names.length]} ${["Smith", "Garcia", "Chen", "Okafor", "Silva"][i % 5]}`,
    tier: ["free", "pro", "enterprise"][i % 3],
    address: { city: c[0], location: { type: "Point", coordinates: [c[1] + (i % 50) / 500, c[2] + (i % 37) / 500] } },
    tags: i % 4 === 0 ? ["beta", "newsletter"] : ["newsletter"],
    signupAt: new Date(Date.UTC(2023, 0, 1) + i * 3600e3),
    ...(i % 7 === 0 ? { referral: { code: `R${i}`, bonus: NumberDecimal("5.00") } } : {}),
  });
}
const ids = db.customers.insertMany(customers).insertedIds;
db.customers.createIndex({ email: 1 }, { unique: true });
db.customers.createIndex({ "address.location": "2dsphere" });
const orders = [];
for (let i = 0; i < 8000; i++) {
  orders.push({
    customerId: ids[i % 2000],
    status: ["pending", "paid", "shipped", "delivered", "refunded"][i % 5],
    total: NumberDecimal(((i * 7.31) % 900 + 10).toFixed(2)),
    items: [{ sku: `SKU-${i % 400}`, qty: 1 + (i % 3) }, ...(i % 2 ? [{ sku: `SKU-${(i * 3) % 400}`, qty: 1 }] : [])],
    placedAt: new Date(Date.UTC(2024, 0, 1) + i * 780e3),
  });
}
db.orders.insertMany(orders);
db.orders.createIndex({ customerId: 1, placedAt: -1 });
db.orders.createIndex({ status: 1 });
db.createView("paid_orders", "orders", [{ $match: { status: "paid" } }, { $project: { customerId: 1, total: 1, placedAt: 1 } }]);
print("seeded", db.customers.countDocuments(), "customers and", db.orders.countDocuments(), "orders");
