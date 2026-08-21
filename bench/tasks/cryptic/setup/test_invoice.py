from app.invoice import invoice_total

def main():
    items = [{"qty": 2, "unit_price": 10.0}, {"qty": 1, "unit_price": 5.0}]
    assert invoice_total(items) == 27.0, invoice_total(items)
    assert invoice_total(items, {"include_tax": False}) == 25.0
    print("ok")

main()
